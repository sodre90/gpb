package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpb/internal/config"
	"gpb/internal/store"
)

// backUpATwin leaves the data directory holding one backed-up file and one written-off copy
// of it, and returns the written-off file's path.
func backUpATwin(t *testing.T) string {
	t.Helper()

	dataDir := t.TempDir()
	t.Setenv("GPB_DATA_DIR", dataDir)
	t.Setenv("GPB_PHOTOS_DIR", filepath.Join(t.TempDir(), "photos"))

	cfg, err := config.Load(config.DataDir())
	if err != nil {
		t.Fatalf("loading the config: %v", err)
	}

	const body = "photo a bytes"
	pool := t.TempDir()
	digest := sha256.Sum256([]byte(body))
	err = withStore(cfg, func(db *store.Store) error {
		at := time.Now()
		for _, key := range []string{"kept", "twin"} {
			path := filepath.Join(pool, key+".jpg")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				return err
			}
			item := store.MediaItem{MediaKey: key, Filename: "IMAG0003.jpg", CapturedAt: at}
			if err := db.UpsertItem(item, at); err != nil {
				return err
			}
			item.LocalPath, item.SizeBytes, item.SHA256 = path, int64(len(body)), hex.EncodeToString(digest[:])
			if err := db.MarkDownloaded(item, at); err != nil {
				return err
			}
		}
		return db.MarkMissingUpstream("twin", at)
	})
	if err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	return filepath.Join(pool, "twin.jpg")
}

func TestDuplicatesCommandListsWithoutDeletingUnlessAsked(t *testing.T) {
	twin := backUpATwin(t)

	var err error
	printed := capture(t, func() { err = runDuplicates(t.Context(), nil) })
	if err != nil {
		t.Fatalf("listing the duplicates: %v", err)
	}
	if !strings.Contains(printed, "1 written-off files are copies") || !strings.Contains(printed, twin) {
		t.Fatalf("the listing reads:\n%s", printed)
	}
	if _, err := os.Stat(twin); err != nil {
		t.Fatal("the listing removed the written-off file")
	}

	printed = capture(t, func() { err = runDuplicates(t.Context(), []string{"--delete"}) })
	if err != nil {
		t.Fatalf("removing the duplicates: %v", err)
	}
	if !strings.Contains(printed, "1 written-off files removed") {
		t.Fatalf("the report reads:\n%s", printed)
	}
	if _, err := os.Stat(twin); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the written-off file is still there after --delete (%v)", err)
	}
}
