package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
)

// hookedDaemon records every notification in a file, through a real hook script, so a test can
// read back what the user would have been told.
func hookedDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()

	daemon := scheduledDaemon(t, "03:30")
	heard := filepath.Join(t.TempDir(), "heard")
	hook := filepath.Join(t.TempDir(), "hook.sh")
	script := "#!/bin/sh\necho \"$1 $2\" >> " + heard + "\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the hook: %v", err)
	}
	daemon.cfg.Notify.Command = hook
	return daemon, heard
}

func backUpFile(t *testing.T, db *store.Store, key, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), key+".jpg")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing the pool file: %v", err)
	}
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertItem(store.MediaItem{MediaKey: key, Filename: key + ".jpg", CapturedAt: at}, at); err != nil {
		t.Fatalf("seeding the item: %v", err)
	}
	digest := sha256.Sum256([]byte(body))
	downloaded := store.MediaItem{MediaKey: key, Filename: key + ".jpg", LocalPath: path,
		SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(digest[:])}
	if err := db.MarkDownloaded(downloaded, at); err != nil {
		t.Fatalf("marking downloaded: %v", err)
	}
	return path
}

func readOrEmpty(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(content)
}

func TestASweepThatFindsNothingIsRecordedQuietly(t *testing.T) {
	daemon, heard := hookedDaemon(t)
	backUpFile(t, daemon.store, "intact", "the photograph")

	daemon.verifyIfOwed(context.Background(), time.Now())

	marker := readOrEmpty(t, filepath.Join(daemon.cfg.DataDir(), verifyMarkerFileName))
	if !strings.Contains(marker, "1 files checked, 1 intact") {
		t.Errorf("the marker says %q, want the sweep's report", marker)
	}
	if said := readOrEmpty(t, heard); said != "" {
		t.Errorf("a clean sweep paged the user: %q", said)
	}
}

func TestASweepThatFindsDamageSaysSoAndRepairsNothing(t *testing.T) {
	daemon, heard := hookedDaemon(t)
	path := backUpFile(t, daemon.store, "rotted", "the photograph")
	if err := os.WriteFile(path, []byte("the photograph, rotted"), 0o644); err != nil {
		t.Fatalf("rotting the file: %v", err)
	}

	daemon.verifyIfOwed(context.Background(), time.Now())

	said := readOrEmpty(t, heard)
	if !strings.HasPrefix(said, "verify_problems ") || !strings.Contains(said, "1 corrupt") {
		t.Errorf("the hook heard %q, want a verify_problems naming the corrupt file", said)
	}
	item, err := daemon.store.Item("rotted")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if item.State != store.StateDone {
		t.Errorf("the scheduled sweep moved a damaged file to %s; repairing is left to a person", item.State)
	}
}

func TestARecentSweepIsNotRepeated(t *testing.T) {
	daemon, _ := hookedDaemon(t)
	marker := filepath.Join(daemon.cfg.DataDir(), verifyMarkerFileName)
	if err := os.WriteFile(marker, []byte("last month's report\n"), 0o600); err != nil {
		t.Fatalf("planting the marker: %v", err)
	}
	backUpFile(t, daemon.store, "intact", "the photograph")

	daemon.verifyIfOwed(context.Background(), time.Now())

	if got := readOrEmpty(t, marker); got != "last month's report\n" {
		t.Errorf("a sweep ran though the last one was moments ago; the marker now says %q", got)
	}
	if !fileOlderThan(marker, verifyInterval, time.Now().Add(verifyInterval+time.Minute)) {
		t.Error("a sweep a month old was not treated as owed")
	}
}
