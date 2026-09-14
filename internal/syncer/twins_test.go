package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
)

// A written-off file whose photo is still backed up is listed with the copy it duplicates,
// and nothing is touched until --delete is asked for.
func TestTheDryRunNamesEveryTwinAndRemovesNothing(t *testing.T) {
	db, pool := openStoreWithTwins(t)

	cleanup, err := RemoveTwins(t.Context(), db, false)
	if err != nil {
		t.Fatalf("listing the twins: %v", err)
	}
	if len(cleanup.Removals) != 2 || cleanup.Removed != 0 || cleanup.Refused != 0 {
		t.Fatalf("the dry run found %+v, want two twins and nothing done", cleanup)
	}
	lines := cleanup.Removals[0].String() + "\n" + cleanup.Removals[1].String()
	for _, want := range []string{"is the same bytes as", "is a smaller file of the same name and second as", pool} {
		if !strings.Contains(lines, want) {
			t.Errorf("the listing does not say %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "removed") {
		t.Errorf("the dry run claims to have removed something:\n%s", lines)
	}
	if !strings.Contains(cleanup.String(), "2 written-off files are copies") || !strings.Contains(cleanup.String(), "--delete") {
		t.Errorf("the summary is %q, want it to count the twins and say how to remove them", cleanup)
	}
	for _, name := range []string{"identical-twin.jpg", "smaller-twin.jpg", "kept-a.jpg", "kept-b.jpg"} {
		if _, err := os.Stat(filepath.Join(pool, name)); err != nil {
			t.Errorf("%s is gone after a dry run", name)
		}
	}
}

// With --delete the written-off file goes, its row goes with it, and the copy that stays is
// left exactly as it was.
func TestDeleteRemovesTheTwinAndKeepsTheCopy(t *testing.T) {
	db, pool := openStoreWithTwins(t)

	cleanup, err := RemoveTwins(t.Context(), db, true)
	if err != nil {
		t.Fatalf("removing the twins: %v", err)
	}
	if cleanup.Removed != 2 || cleanup.Refused != 0 {
		t.Fatalf("the pass did %+v, want both twins removed", cleanup)
	}
	if !strings.Contains(cleanup.String(), "2 written-off files removed") {
		t.Errorf("the summary is %q, want it to say what was removed", cleanup)
	}
	for _, name := range []string{"identical-twin.jpg", "smaller-twin.jpg"} {
		if _, err := os.Stat(filepath.Join(pool, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still on disk after --delete (%v)", name, err)
		}
	}
	for _, name := range []string{"kept-a.jpg", "kept-b.jpg"} {
		if _, err := os.Stat(filepath.Join(pool, name)); err != nil {
			t.Errorf("the kept copy %s is gone", name)
		}
	}
	for _, key := range []string{"identical-twin", "smaller-twin"} {
		if _, err := db.Item(key); err == nil {
			t.Errorf("the row for %s is still there after --delete", key)
		}
	}
	if kept, err := db.Item("kept-a"); err != nil || kept.State != store.StateDone {
		t.Errorf("the kept copy's row is %+v (%v), want it untouched", kept, err)
	}

	again, err := RemoveTwins(t.Context(), db, true)
	if err != nil || len(again.Removals) != 0 {
		t.Errorf("a second pass found %+v (%v), want nothing left to do", again, err)
	}
}

// The kept copy is re-read before anything is deleted. One that no longer hashes to what was
// recorded is not a copy anyone can rely on, so its twin stays too.
func TestAKeptCopyThatDoesNotMatchItsHashKeepsItsTwin(t *testing.T) {
	db, pool := openStoreWithTwins(t)
	if err := os.WriteFile(filepath.Join(pool, "kept-a.jpg"), []byte("rotted"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanup, err := RemoveTwins(t.Context(), db, true)
	if err != nil {
		t.Fatalf("removing the twins: %v", err)
	}
	if cleanup.Removed != 1 || cleanup.Refused != 1 {
		t.Fatalf("the pass did %+v, want one removed and one refused", cleanup)
	}
	refused := cleanup.Removals[0]
	if !strings.Contains(refused.String(), "left alone: the kept copy no longer hashes") {
		t.Errorf("the refusal reads %q", refused)
	}
	if _, err := os.Stat(filepath.Join(pool, "identical-twin.jpg")); err != nil {
		t.Errorf("the twin of a corrupt copy was removed")
	}
	if _, err := db.Item("identical-twin"); err != nil {
		t.Errorf("the twin's row went even though its file stayed: %v", err)
	}
	if !strings.Contains(cleanup.String(), "1 left alone") {
		t.Errorf("the summary is %q, want it to count the refusal", cleanup)
	}
}

// openStoreWithTwins builds a pool holding two written-off files with a backed-up copy each:
// identical-twin is byte for byte kept-a, and smaller-twin is a smaller file with the same name
// and capture second as kept-b.
func openStoreWithTwins(t *testing.T) (*store.Store, string) {
	t.Helper()

	pool := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	noon := time.Date(2013, 6, 26, 12, 0, 0, 0, time.UTC)
	files := []struct {
		key, filename, body string
	}{
		{"kept-a", "IMAG0003.jpg", "the photo of the beach"},
		{"identical-twin", "IMAG0003.jpg", "the photo of the beach"},
		{"kept-b", "IMAG0004.jpg", "the photo of the harbour, at full size"},
		{"smaller-twin", "IMAG0004.jpg", "the harbour, re-encoded"},
	}
	for _, file := range files {
		path := filepath.Join(pool, file.key+".jpg")
		if err := os.WriteFile(path, []byte(file.body), 0o600); err != nil {
			t.Fatal(err)
		}
		item := store.MediaItem{MediaKey: file.key, Filename: file.filename, CapturedAt: noon}
		if err := db.UpsertItem(item, noon); err != nil {
			t.Fatalf("seeding %s: %v", file.key, err)
		}
		digest := sha256.Sum256([]byte(file.body))
		item.LocalPath, item.SizeBytes, item.SHA256 = path, int64(len(file.body)), hex.EncodeToString(digest[:])
		if err := db.MarkDownloaded(item, noon); err != nil {
			t.Fatalf("marking %s downloaded: %v", file.key, err)
		}
	}
	for _, key := range []string{"identical-twin", "smaller-twin"} {
		if err := db.MarkMissingUpstream(key, noon.AddDate(13, 0, 0)); err != nil {
			t.Fatalf("writing off %s: %v", key, err)
		}
	}
	return db, pool
}
