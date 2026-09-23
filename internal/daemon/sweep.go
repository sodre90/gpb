package daemon

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"gpb/internal/syncer"
)

// verifyMarkerFileName records when the pool was last re-read in full, by its modification time,
// and what that sweep found, in its one line of text. The container has no cron to run
// `gpb verify` monthly, so without this nothing would ever read a finished file again.
const verifyMarkerFileName = "verify.last"

// verifyInterval is long because a sweep is hours of reading: 1.4 TB on the disk this was written
// for, a disk it shares with the nightly run and with other tenants.
const verifyInterval = 30 * 24 * time.Hour

const verifyCheckInterval = time.Hour

// verifyOnSchedule asks first an hour after startup rather than at it: a first sweep on an
// install that has never had one would otherwise start reading the whole pool the moment the
// daemon comes up, alongside the startup warmup and the locator.
func (d *Daemon) verifyOnSchedule(ctx context.Context) {
	ticker := time.NewTicker(verifyCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			d.verifyIfOwed(ctx, now)
		}
	}
}

// verifyIfOwed never repairs. Queuing files to be fetched again is left to a person running
// `gpb verify --repair`, who can first see whether the fault is the files or the mount.
func (d *Daemon) verifyIfOwed(ctx context.Context, now time.Time) {
	marker := filepath.Join(d.cfg.DataDir(), verifyMarkerFileName)
	if d.runner.Activity() != "" || !fileOlderThan(marker, verifyInterval, now) {
		return
	}

	report, err := syncer.Verify(ctx, d.store, false)
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		log.Printf("daemon: the monthly re-read of the backup failed: %v", err)
		d.Notify("verify_failed", "The monthly re-read of the backup could not finish: "+err.Error())
		return
	}

	log.Printf("daemon: monthly re-read of the backup: %s", report)
	if err := os.WriteFile(marker, []byte(report.String()+"\n"), 0o600); err != nil {
		log.Printf("daemon: recording the re-read: %v", err)
	}
	if len(report.Problems) > 0 {
		d.Notify("verify_problems", "The monthly re-read of the backup found damage: "+report.String()+
			". `gpb verify` lists the files, and `gpb verify --repair` fetches them again.")
	}
}
