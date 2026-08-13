package daemon

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"gpb/internal/auth"
	"gpb/internal/config"
	"gpb/internal/engine"
	"gpb/internal/links"
	"gpb/internal/store"
	"gpb/internal/syncer"
)

// runTimeout bounds a whole backup pass. It is generous because the pass is deliberately
// slow — a few requests a second against a large library — but it is not absent: a run that
// has hung for half a day is stuck, and holding the slot forever would block every later one.
const runTimeout = 12 * time.Hour

// Runner owns the one-run-at-a-time rule. The daemon and the web UI both ask it for work,
// and it is the only thing in the process that starts a sync, so the guard has nowhere to
// leak from.
type Runner struct {
	cfg   config.Config
	auth  *auth.Manager
	store *store.Store

	// notify is how a run that stopped for a reason nobody is watching for reaches the user. It
	// is optional so a Runner can be built and driven without one.
	notify func(event, message string)

	mu       sync.Mutex
	activity string
	cancel   context.CancelFunc
	// current is the pass in flight, held only so the UI can ask it how far it has got. It is
	// nil between runs and for the moment a claimed run spends connecting to Google.
	current  *syncer.Syncer
	inFlight sync.WaitGroup
}

func NewRunner(cfg config.Config, manager *auth.Manager, db *store.Store) *Runner {
	return &Runner{cfg: cfg, auth: manager, store: db}
}

// StartSync begins a backup pass and returns immediately: a pass takes minutes to hours,
// and the HTTP request that asked for it cannot wait that long.
func (r *Runner) StartSync(reason string) error {
	return r.start("sync", reason, r.recordFailureToStart, func(ctx context.Context, pass *syncer.Syncer) error {
		report, err := pass.Run(ctx)
		log.Printf("daemon: sync finished %s — %d listed, %d downloaded, %d failed",
			report.Outcome, report.Listed, report.Downloaded, report.Failed)

		r.reportIfOutOfDisk(err)
		r.relinkAlbums()
		return err
	})
}

// reportIfOutOfDisk pushes the one failure that otherwise passes for success. A backup stopped
// for want of disk leaves no sign a person will meet on their own — the UI has to be opened to
// see it, and every later run stops the same silent way. It is worth a notification for the
// reason a signed-out session is: nothing but a person can clear it.
func (r *Runner) reportIfOutOfDisk(err error) {
	if errors.Is(err, syncer.ErrDiskFull) {
		r.report("disk_full", "backup stopped: "+err.Error())
	}
}

func (r *Runner) report(event, message string) {
	if r.notify == nil {
		log.Printf("event: %s — %s", event, message)
		return
	}
	r.notify(event, message)
}

// StartRefresh re-reads the album list and nothing else. It is what makes the album picker
// usable on a fresh install, where there is nothing to pick from until Google is asked.
func (r *Runner) StartRefresh(reason string) error {
	return r.start("album refresh", reason, notWorthARunRow, func(ctx context.Context, pass *syncer.Syncer) error {
		return pass.RefreshAlbums(ctx)
	})
}

// StartAlbumListing walks one album's contents and downloads nothing. It is what makes the
// item grid usable: nothing lists an album's items until it is followed, so without this a
// user switching an album to "picked" would be shown an empty grid and no way to fill it.
func (r *Runner) StartAlbumListing(albumID, reason string) error {
	return r.start("album listing", reason, notWorthARunRow, func(ctx context.Context, pass *syncer.Syncer) error {
		listed, err := pass.ListAlbum(ctx, albumID)
		log.Printf("daemon: album listing finished — %d items", listed)
		return err
	})
}

// relinkAlbums refreshes the browsable album view. It runs even when the pass failed, since
// a partial run still downloaded files worth linking, and a failure here is logged rather
// than returned: the backup succeeded, and a convenience view is no reason to call it broken.
func (r *Runner) relinkAlbums() {
	report, err := links.Rebuild(r.store, r.cfg.PhotosDir)
	if err != nil {
		log.Printf("daemon: could not rebuild the album view: %v", err)
		return
	}
	log.Printf("daemon: album view rebuilt — %s", report)
}

// Activity names what is running, or "" when nothing is. The UI uses it both to disable the
// buttons and to decide whether the page should poll itself.
func (r *Runner) Activity() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activity
}

// Progress is how far the run in flight has got. Between runs it is zero, which the UI reads
// as "nothing to show" — the durable counts of the last finished run come from the store.
func (r *Runner) Progress() syncer.Progress {
	r.mu.Lock()
	pass := r.current
	r.mu.Unlock()

	if pass == nil {
		return syncer.Progress{}
	}
	return pass.Progress()
}

// Stop cancels a run in flight and waits for it to unwind. The wait is what makes shutdown
// safe rather than merely quick: the run writes to the store as it goes, and closing the
// database out from under it would turn an orderly stop into a pile of errors. A cancelled
// pass loses nothing — every finished item is already committed, and the next run resumes.
func (r *Runner) Stop() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()

	r.inFlight.Wait()
}

func (r *Runner) start(activity, reason string, onFailureToStart func(error),
	work func(context.Context, *syncer.Syncer) error) error {
	ctx, err := r.claim(activity)
	if err != nil {
		return err
	}

	log.Printf("daemon: %s starting (%s)", activity, reason)
	go func() {
		defer r.release()
		if err := r.run(ctx, onFailureToStart, work); err != nil {
			log.Printf("daemon: %s failed: %v", activity, err)
		}
	}()
	return nil
}

// notWorthARunRow is for the jobs that are not the backup. An album refresh that could not
// reach Google is a page saying so, not an entry in the backup history.
func notWorthARunRow(error) {}

// recordFailureToStart writes the run that never got as far as reporting for itself. A backup
// that dies taking the lock, warming the profile or connecting to Google has still happened as
// far as anyone waiting for it is concerned, and leaving no row made it invisible twice over:
// absent from the runs page, and absent from the schedule, which went on owing a backup and
// asking for it again every tick — measured at roughly 1,400 browser launches a day.
func (r *Runner) recordFailureToStart(cause error) {
	at := time.Now()
	runID, err := r.store.StartRun(at)
	if err != nil {
		log.Printf("daemon: could not record a run that failed to start: %v", err)
		return
	}
	if err := r.store.FinishRun(store.SyncRun{
		ID:      runID,
		Outcome: syncer.OutcomeOf(cause),
		Error:   cause.Error(),
	}, at); err != nil {
		log.Printf("daemon: could not record a run that failed to start: %v", err)
	}
}

// claim takes the in-process slot before anything expensive happens, so a double-click on
// "Sync now" is refused in microseconds rather than after starting a browser.
func (r *Runner) claim(activity string) (context.Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.activity != "" {
		return nil, syncer.ErrRunInProgress
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	r.activity, r.cancel = activity, cancel
	r.inFlight.Add(1)
	return ctx, nil
}

func (r *Runner) release() {
	r.mu.Lock()
	r.cancel()
	r.activity, r.cancel, r.current = "", nil, nil
	r.mu.Unlock()

	r.inFlight.Done()
}

// run holds the cross-process lock for the whole pass, not just the setup: the point is to
// exclude a `podman exec gpb gpb sync` that would otherwise share the pool.
func (r *Runner) run(ctx context.Context, onFailureToStart func(error),
	work func(context.Context, *syncer.Syncer) error) error {
	unlock, err := engine.LockRun(r.cfg)
	if err != nil {
		onFailureToStart(err)
		return err
	}
	defer unlock()

	syncEngine, err := engine.Connect(ctx, r.auth, r.store, engine.Options(r.cfg))
	if err != nil {
		onFailureToStart(err)
		return err
	}

	r.mu.Lock()
	r.current = syncEngine
	r.mu.Unlock()

	return work(ctx, syncEngine)
}
