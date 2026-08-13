package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestADatabaseCopyIsOwedUntilOneExists(t *testing.T) {
	daemon := scheduledDaemon(t, "03:30")
	path := filepath.Join(daemon.cfg.DataDir(), databaseCopyFileName)

	if !databaseCopyOwed(path, time.Now()) {
		t.Fatal("an install with no copy of its database was owed none")
	}

	daemon.copyDatabaseIfOwed(time.Now())
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no copy was written: %v", err)
	}
}

// The whole point of reading the file's age rather than counting from a timer: the daemon this
// runs on is restarted whenever the image is rebuilt, and a weekly deadline counted in memory
// would be reset every time and never fall due.
func TestTheAgeOfTheCopyOnDiskIsWhatDecides(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseCopyFileName)
	if err := os.WriteFile(path, []byte("a copy"), 0o600); err != nil {
		t.Fatalf("planting a copy: %v", err)
	}

	now := time.Now()
	if databaseCopyOwed(path, now) {
		t.Error("a copy taken moments ago was treated as overdue")
	}
	if !databaseCopyOwed(path, now.Add(databaseCopyInterval+time.Minute)) {
		t.Error("a copy older than the interval was not treated as overdue")
	}
}

func TestADueCopyReplacesTheOneBefore(t *testing.T) {
	daemon := scheduledDaemon(t, "03:30")
	path := filepath.Join(daemon.cfg.DataDir(), databaseCopyFileName)

	if err := os.WriteFile(path, []byte("last week's copy"), 0o600); err != nil {
		t.Fatalf("planting last week's copy: %v", err)
	}
	stale := time.Now().Add(-databaseCopyInterval - time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatalf("ageing last week's copy: %v", err)
	}

	daemon.copyDatabaseIfOwed(time.Now())

	copied, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the copy: %v", err)
	}
	if string(copied) == "last week's copy" {
		t.Fatal("the overdue copy was not replaced")
	}
}
