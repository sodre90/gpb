package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpb/internal/config"
	"gpb/internal/store"
)

// backUpOneFile leaves the data directory looking like an install with a single finished
// download in it, so runVerify can be driven exactly as a cron entry would drive it.
func backUpOneFile(t *testing.T, body string) string {
	t.Helper()

	dataDir := t.TempDir()
	t.Setenv("GPB_DATA_DIR", dataDir)
	t.Setenv("GPB_PHOTOS_DIR", filepath.Join(t.TempDir(), "photos"))

	cfg, err := config.Load(config.DataDir())
	if err != nil {
		t.Fatalf("loading the config: %v", err)
	}

	path := filepath.Join(t.TempDir(), "key-a.jpg")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing the pool file: %v", err)
	}

	err = withStore(cfg, func(db *store.Store) error {
		at := time.Now()
		if err := db.UpsertItem(store.MediaItem{MediaKey: "key-a", Filename: "key-a.jpg"}, at); err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(body))
		return db.MarkDownloaded(store.MediaItem{
			MediaKey:  "key-a",
			Filename:  "key-a.jpg",
			LocalPath: path,
			SizeBytes: int64(len(body)),
			SHA256:    hex.EncodeToString(digest[:]),
		}, at)
	})
	if err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	return path
}

func TestVerifyCommandSaysNothingIsWrongWhenNothingIs(t *testing.T) {
	backUpOneFile(t, "photo a bytes")

	var err error
	printed := capture(t, func() { err = runVerify(t.Context(), nil) })
	if err != nil {
		t.Fatalf("verifying a sound backup: %v", err)
	}
	if !strings.Contains(printed, "1 files checked, 1 intact") {
		t.Fatalf("the report reads:\n%s", printed)
	}
}

// The exit code is the interface a scheduled sweep has: a cron entry that has to grep the output
// to learn whether the backup is sound is one that will eventually be reading it wrong.
func TestVerifyCommandFailsWhenTheBackupIsDamaged(t *testing.T) {
	path := backUpOneFile(t, "photo a bytes")
	if err := os.WriteFile(path, []byte("rotted a bytes"), 0o644); err != nil {
		t.Fatalf("corrupting the file: %v", err)
	}

	var err error
	printed := capture(t, func() { err = runVerify(t.Context(), nil) })
	if err == nil {
		t.Fatal("a damaged backup verified without an error, so a cron entry would report success")
	}
	if !strings.Contains(printed, "corrupt") || !strings.Contains(printed, "key-a") {
		t.Fatalf("the report does not name the damaged file:\n%s", printed)
	}
}
