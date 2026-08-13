package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gpb/internal/auth"
	"gpb/internal/config"
	"gpb/internal/homeassistant"
	"gpb/internal/store"
	"gpb/internal/version"
	"gpb/internal/web"
)

// postReauthWarmupTimeout is generous: this warmup follows a fresh interactive login, and
// a first page load on a cold profile is the slowest one the daemon ever does.
const postReauthWarmupTimeout = 3 * time.Minute

// scheduleInterval is how often the daemon asks whether a backup is owed. The question costs one
// indexed row and a look at the clock, and asking it often is what lets a daemon that was
// restarted — or was simply down when the hour came round — pick the schedule up promptly
// instead of waiting out a whole day.
const scheduleInterval = time.Minute

const databaseFileName = "state.db"

type Daemon struct {
	cfg    config.Config
	auth   *auth.Manager
	reauth *auth.ReauthStack
	store  *store.Store
	runner *Runner
	web    *web.Server
	bridge *homeassistant.Bridge

	stateMu           sync.Mutex
	lastReportedState auth.State
}

func New(cfg config.Config) (*Daemon, error) {
	if err := os.MkdirAll(cfg.ProfileDir(), 0o700); err != nil {
		return nil, err
	}

	db, err := store.Open(filepath.Join(cfg.DataDir(), databaseFileName))
	if err != nil {
		return nil, err
	}

	manager := auth.NewManager(cfg.ProfileDir())
	reauth := auth.NewReauthStack(manager, cfg.ProfileDir())

	daemon := &Daemon{
		cfg:               cfg,
		auth:              manager,
		reauth:            reauth,
		store:             db,
		runner:            NewRunner(cfg, manager, db),
		lastReportedState: auth.StateUnknown,
	}
	daemon.runner.notify = daemon.Notify
	daemon.bridge = homeassistant.New(daemon.mqttSettings, cfg.Web.ExternalURL, daemon, daemon)
	daemon.web = web.NewServer(web.Deps{
		Config:        cfg,
		Auth:          manager,
		Reauth:        reauth,
		Store:         db,
		Runs:          daemon.runner,
		Notify:        daemon.Notify,
		HomeAssistant: daemon.bridge,
	})
	return daemon, nil
}

func (d *Daemon) Run(ctx context.Context) error {
	log.Printf("daemon: gpb %s", version.Current)

	if !d.cfg.HasPassword() {
		log.Print("daemon: no web password is set; run `gpb passwd` before the UI is usable")
	}

	d.reauth.OnClosed(d.verifyAfterReauth)
	defer d.reauth.Stop("daemon shutting down")

	defer d.store.Close()
	defer d.runner.Stop()

	go d.keepalive(ctx)
	go d.backUpOnSchedule(ctx)
	go d.copyDatabaseOnSchedule(ctx)
	go d.bridge.Run(ctx)

	log.Printf("daemon: web UI on %s over %s", d.cfg.Web.Listen, d.cfg.Web.TLS.Scheme())
	return d.web.ListenAndServe(ctx)
}

func (d *Daemon) backUpOnSchedule(ctx context.Context) {
	ticker := time.NewTicker(scheduleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			d.startBackupIfOwed(now)
		}
	}
}

func (d *Daemon) startBackupIfOwed(now time.Time) {
	reason, owed := d.backupOwed(now)
	if !owed {
		return
	}

	if err := d.runner.StartSync(reason); err != nil {
		log.Printf("daemon: the scheduled backup could not start: %v", err)
	}
}

// backupOwed answers from what the store already records rather than from a timer this process
// holds in memory. A daemon that was restarted has no memory of the hour going past, and the run
// it was killed in the middle of is the one thing nobody else will restart — which is how a
// restart used to stop the backup silently and for good.
func (d *Daemon) backupOwed(now time.Time) (string, bool) {
	if d.runner.Activity() != "" {
		return "", false
	}

	runs, err := d.store.RecentRuns(1)
	if err != nil {
		log.Printf("daemon: could not read the last run: %v", err)
		return "", false
	}

	switch {
	case len(runs) == 0:
		return "nothing has been backed up yet", true
	// Killed outright leaves no finishing time; asked to stop leaves one and says why. Both mean
	// the same thing to a schedule — the backup was cut off rather than answered.
	case runs[0].FinishedAt.IsZero(), runs[0].Outcome == store.OutcomeInterrupted:
		return "the last run was interrupted", true
	case runs[0].StartedAt.Before(d.lastScheduledFor(now)):
		return "scheduled", true
	default:
		return "", false
	}
}

// lastScheduledFor is the most recent moment the backup was meant to begin: today's time of day
// if it has already gone past, and yesterday's if it has not.
func (d *Daemon) lastScheduledFor(now time.Time) time.Time {
	at, err := d.scheduledTimeOfDay()
	if err != nil {
		log.Printf("daemon: the configured backup time is unusable, so nothing is scheduled: %v", err)
		return time.Time{}
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	if today.After(now) {
		return today.AddDate(0, 0, -1)
	}
	return today
}

// scheduledTimeOfDay re-reads the config so that a backup time changed on the settings page takes
// effect without restarting the daemon, the way the web password already does.
func (d *Daemon) scheduledTimeOfDay() (time.Time, error) {
	cfg, err := config.Load(d.cfg.DataDir())
	if err != nil {
		log.Printf("daemon: re-reading the config for the backup time: %v", err)
		return d.cfg.SyncTime()
	}
	return cfg.SyncTime()
}

func (d *Daemon) keepalive(ctx context.Context) {
	d.warmupOnce(ctx, "startup")

	ticker := time.NewTicker(d.cfg.Schedule.KeepaliveInterval.Duration)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.warmupOnce(ctx, "keepalive")
		}
	}
}

func (d *Daemon) warmupOnce(ctx context.Context, trigger string) {
	log.Printf("daemon: %s warmup starting", trigger)
	_, err := d.auth.Warmup(ctx)
	d.reportWarmup(trigger, err)
}

func (d *Daemon) reportWarmup(trigger string, err error) {
	switch {
	case errors.Is(err, auth.ErrProfileBusy):
		log.Printf("daemon: %s warmup skipped, a browser login holds the profile", trigger)
		return
	case err != nil:
		log.Printf("daemon: %s warmup failed: %v", trigger, err)
	default:
		log.Printf("daemon: %s warmup ok", trigger)
	}

	d.reportStateChange()
}

// reportStateChange fires the notify hook only when health flips, so a session that has
// been broken for a week does not page the user every keepalive tick.
func (d *Daemon) reportStateChange() {
	d.stateMu.Lock()
	state := d.auth.Status().State
	unchanged := state == d.lastReportedState
	d.lastReportedState = state
	d.stateMu.Unlock()

	if unchanged {
		return
	}

	switch state {
	case auth.StateAuthRequired:
		d.Notify("auth_required", "Google sign-in needed: "+d.reauthLink())
	case auth.StateWarmupFailed:
		d.Notify("warmup_failed", "The Google session could not be checked: "+d.auth.Status().LastError)
	case auth.StateOK:
		d.Notify("auth_ok", "Google session is healthy again")
	}
}

// reauthLink is where the user has to go, said as precisely as this process can. Only the
// operator knows the address the UI answers on from outside the container, so an unset
// external_url gets the page name rather than a guessed URL that would send them somewhere
// that does not answer.
func (d *Daemon) reauthLink() string {
	if d.cfg.Web.ExternalURL == "" {
		return "open the gpb web UI and go to the Google page"
	}
	return strings.TrimSuffix(d.cfg.Web.ExternalURL, "/") + "/reauth"
}

// verifyAfterReauth returns as soon as the check is registered. It runs on the web handler
// that closed the login browser, which is holding a redirect open — but the warmup itself
// can take a minute, and the page it redirects to has to already know a check is under way.
func (d *Daemon) verifyAfterReauth(reason string) {
	log.Printf("daemon: post-reauth warmup starting (%s)", reason)

	ctx, cancel := context.WithTimeout(context.Background(), postReauthWarmupTimeout)
	d.auth.WarmupInBackground(ctx, func(_ *auth.Session, err error) {
		defer cancel()
		d.reportWarmup("post-reauth", err)
		d.Notify("reauth_finished", "browser login closed ("+reason+"); session is now "+string(d.auth.Status().State))
	})
}
