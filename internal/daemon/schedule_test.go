package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gpb/internal/config"
	"gpb/internal/store"
)

func scheduledDaemon(t *testing.T, at string) *Daemon {
	t.Helper()

	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("loading a config: %v", err)
	}
	cfg.Schedule.SyncAt = at
	if err := cfg.Save(); err != nil {
		t.Fatalf("saving the config: %v", err)
	}

	db, err := store.Open(filepath.Join(cfg.DataDir(), databaseFileName))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return &Daemon{cfg: cfg, store: db, runner: NewRunner(cfg, nil, db)}
}

func TestAnAccountThatHasNeverRunIsOwedABackup(t *testing.T) {
	daemon := scheduledDaemon(t, "03:30")

	reason, owed := daemon.backupOwed(time.Now())
	if !owed {
		t.Fatal("a store with nothing in it was owed no backup")
	}
	if reason == "" {
		t.Error("the run would have been started without saying why")
	}
}

// This is the failure the schedule was written for: a run killed by a restart is the one nobody
// else restarts, and waiting for tomorrow's hour meant a whole day backing nothing up.
func TestAnInterruptedRunIsPickedBackUp(t *testing.T) {
	daemon := scheduledDaemon(t, "03:30")

	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.Local)
	if _, err := daemon.store.StartRun(now.Add(-2 * time.Hour)); err != nil {
		t.Fatalf("recording the interrupted run: %v", err)
	}

	reason, owed := daemon.backupOwed(now)
	if !owed {
		t.Fatal("a run that never finished was left for dead")
	}
	if reason != "the last run was interrupted" {
		t.Errorf("the reason given was %q", reason)
	}
}

// A daemon told to stop finishes the run it was in and says why, so the row is not the blank
// finishing time a kill leaves behind. Reading only that blank is what left the box idle for a
// day after each deploy: today's window had a run against it, and the run had been cut off.
func TestARunStoppedByAShutdownIsPickedBackUp(t *testing.T) {
	daemon := scheduledDaemon(t, "03:30")

	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.Local)
	id, err := daemon.store.StartRun(now.Add(-2 * time.Hour))
	if err != nil {
		t.Fatalf("recording the run: %v", err)
	}
	if err := daemon.store.FinishRun(store.SyncRun{ID: id, Outcome: store.OutcomeInterrupted}, now.Add(-time.Hour)); err != nil {
		t.Fatalf("finishing the run: %v", err)
	}

	if reason, owed := daemon.backupOwed(now); !owed {
		t.Error("a run cut off by a restart was left until tomorrow")
	} else if reason != "the last run was interrupted" {
		t.Errorf("the reason given was %q", reason)
	}
}

// A run that failed for its own reasons is not retried on the next tick: whatever broke it would
// most likely break the retry, once a minute, all day.
func TestARunThatFailedIsNotRetriedUntilTheNextWindow(t *testing.T) {
	daemon := scheduledDaemon(t, "03:30")

	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.Local)
	id, err := daemon.store.StartRun(now.Add(-2 * time.Hour))
	if err != nil {
		t.Fatalf("recording the run: %v", err)
	}
	if err := daemon.store.FinishRun(store.SyncRun{ID: id, Outcome: store.OutcomeError}, now.Add(-time.Hour)); err != nil {
		t.Fatalf("finishing the run: %v", err)
	}

	if _, owed := daemon.backupOwed(now); owed {
		t.Error("a failed run was started again straight away")
	}
}

// A backup that dies before it can write its own row — the profile held by another process,
// Chrome missing, no network yet — used to leave nothing behind at all: nothing on the runs
// page, and nothing for the schedule, which went on owing a backup and asked for it again on
// the very next tick, a minute later, all day.
func TestABackupThatFailsToStartIsRecordedAndNotRetriedEveryTick(t *testing.T) {
	now := time.Now()
	daemon := scheduledDaemon(t, now.Add(-2*time.Minute).Format("15:04"))

	if _, owed := daemon.backupOwed(now); !owed {
		t.Fatal("the scheduled backup was not owed to begin with")
	}

	daemon.runner.recordFailureToStart(errors.New("chrome could not be started"))

	if _, owed := daemon.backupOwed(now.Add(scheduleInterval)); owed {
		t.Error("the next tick asked for the same backup again")
	}

	runs, err := daemon.store.RecentRuns(1)
	if err != nil {
		t.Fatalf("reading the runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d runs were recorded, want the one that failed to start", len(runs))
	}
	if runs[0].Outcome != store.OutcomeError || runs[0].Error == "" {
		t.Errorf("the failed run is recorded as %q: %q", runs[0].Outcome, runs[0].Error)
	}
	if runs[0].FinishedAt.IsZero() {
		t.Error("a run that failed to start is left looking like one still going")
	}
}

// Shutting the daemon down while a run is still connecting is not that run failing. Recorded as
// an error it would be waited out, and the backup the restart interrupted would not be picked
// back up until the next window.
func TestAShutdownDuringSetupIsRecordedAsInterrupted(t *testing.T) {
	now := time.Now()
	daemon := scheduledDaemon(t, now.Add(-2*time.Minute).Format("15:04"))

	daemon.runner.recordFailureToStart(fmt.Errorf("connecting to google: %w", context.Canceled))

	reason, owed := daemon.backupOwed(now.Add(scheduleInterval))
	if !owed {
		t.Fatal("a backup cut off by a shutdown was left until the next window")
	}
	if reason != "the last run was interrupted" {
		t.Errorf("the reason given was %q", reason)
	}
}

func TestABackupIsOwedOnceTheHourHasGonePast(t *testing.T) {
	for name, testCase := range map[string]struct {
		ranAt time.Time
		now   time.Time
		owed  bool
	}{
		"already run since the hour": {
			ranAt: time.Date(2026, 8, 11, 3, 31, 0, 0, time.Local),
			now:   time.Date(2026, 8, 11, 12, 0, 0, 0, time.Local),
			owed:  false,
		},
		"yesterday's run, hour gone past": {
			ranAt: time.Date(2026, 8, 10, 3, 31, 0, 0, time.Local),
			now:   time.Date(2026, 8, 11, 12, 0, 0, 0, time.Local),
			owed:  true,
		},
		"yesterday's run, hour not here yet": {
			ranAt: time.Date(2026, 8, 10, 3, 31, 0, 0, time.Local),
			now:   time.Date(2026, 8, 11, 2, 0, 0, 0, time.Local),
			owed:  false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			daemon := scheduledDaemon(t, "03:30")
			finished(t, daemon, testCase.ranAt)

			if _, owed := daemon.backupOwed(testCase.now); owed != testCase.owed {
				t.Errorf("owed was %v, want %v", owed, testCase.owed)
			}
		})
	}
}

func TestNothingIsOwedWhileSomethingIsAlreadyRunning(t *testing.T) {
	daemon := scheduledDaemon(t, "03:30")
	if _, err := daemon.runner.claim("sync"); err != nil {
		t.Fatalf("claiming the runner: %v", err)
	}
	defer daemon.runner.release()

	if _, owed := daemon.backupOwed(time.Now()); owed {
		t.Error("a backup was owed on top of the run already in flight")
	}
}

// The settings page writes the time to the config file, so a daemon that only read it at startup
// would go on backing up at the old hour while the page claimed otherwise.
func TestTheBackupTimeIsRereadRatherThanRememberedFromStartup(t *testing.T) {
	daemon := scheduledDaemon(t, "03:30")

	changed := daemon.cfg
	changed.Schedule.SyncAt = "22:15"
	if err := changed.Save(); err != nil {
		t.Fatalf("saving the changed config: %v", err)
	}

	at, err := daemon.scheduledTimeOfDay()
	if err != nil {
		t.Fatalf("re-reading the backup time: %v", err)
	}
	if at.Hour() != 22 || at.Minute() != 15 {
		t.Errorf("the daemon still backs up at %02d:%02d", at.Hour(), at.Minute())
	}
}

// An hour that cannot be parsed must not be read as midnight, which would be owed all day long.
func TestAnUnusableBackupTimeSchedulesNothing(t *testing.T) {
	daemon := scheduledDaemon(t, "half past three")

	if scheduled := daemon.lastScheduledFor(time.Now()); !scheduled.IsZero() {
		t.Errorf("an unparseable time scheduled a backup for %s", scheduled)
	}
}

func finished(t *testing.T, daemon *Daemon, at time.Time) {
	t.Helper()

	id, err := daemon.store.StartRun(at)
	if err != nil {
		t.Fatalf("recording a run: %v", err)
	}
	if err := daemon.store.FinishRun(store.SyncRun{ID: id, Outcome: store.OutcomeOK}, at.Add(time.Hour)); err != nil {
		t.Fatalf("finishing a run: %v", err)
	}
}
