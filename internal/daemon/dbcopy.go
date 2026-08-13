package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

// databaseCopyFileName sits beside the database it is a copy of. The pool is the backup and can
// be re-listed from Google if it has to be; the database is the only record of what was picked,
// what was reviewed and what every run did, and none of that comes back from anywhere.
const databaseCopyFileName = "state.backup.db"

const databaseCopyInterval = 7 * 24 * time.Hour

// databaseCopyCheckInterval is how often the question is asked, not how often a copy is taken.
// It is short so that a daemon which is only up for part of the day still reaches its weekly
// deadline within an hour of it falling due.
const databaseCopyCheckInterval = time.Hour

func (d *Daemon) copyDatabaseOnSchedule(ctx context.Context) {
	d.copyDatabaseIfOwed(time.Now())

	ticker := time.NewTicker(databaseCopyCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			d.copyDatabaseIfOwed(now)
		}
	}
}

// copyDatabaseIfOwed reads the age of the last copy off the disk rather than counting from a
// timer this process holds, for the reason backupOwed does the same: a daemon restarted daily
// never reaches a weekly deadline it only counts while it is up.
func (d *Daemon) copyDatabaseIfOwed(now time.Time) {
	path := filepath.Join(d.cfg.DataDir(), databaseCopyFileName)
	if !databaseCopyOwed(path, now) {
		return
	}

	if err := d.store.BackupTo(path); err != nil {
		log.Printf("daemon: the database copy failed: %v", err)
		d.Notify("db_copy_failed", "The weekly database copy failed: "+err.Error())
		return
	}
	log.Printf("daemon: database copied to %s", path)
}

func databaseCopyOwed(path string, now time.Time) bool {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return true
	case err != nil:
		// Unreadable is not overdue. Copying on the strength of a stat that failed would rewrite
		// the one good copy every hour, which is the opposite of what this is for.
		log.Printf("daemon: could not read the age of %s: %v", path, err)
		return false
	default:
		return now.Sub(info.ModTime()) >= databaseCopyInterval
	}
}
