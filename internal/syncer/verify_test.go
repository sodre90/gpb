package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpb/internal/store"
)

type verifyFixture struct {
	store *store.Store
	pool  string
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()

	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	pool := filepath.Join(root, "pool")
	if err := os.MkdirAll(pool, 0o755); err != nil {
		t.Fatalf("making the pool: %v", err)
	}
	return &verifyFixture{store: db, pool: pool}
}

// backUp puts a file in the pool and records it exactly as a finished download would, hash and
// all — which is what makes a later mismatch mean corruption rather than a test artefact.
func (f *verifyFixture) backUp(t *testing.T, key, body string) string {
	t.Helper()

	path := filepath.Join(f.pool, key+".jpg")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing the pool file: %v", err)
	}

	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	if err := f.store.UpsertItem(store.MediaItem{MediaKey: key, Filename: key + ".jpg", CapturedAt: at}, at); err != nil {
		t.Fatalf("seeding the item: %v", err)
	}
	digest := sha256.Sum256([]byte(body))
	downloaded := store.MediaItem{
		MediaKey:  key,
		Filename:  key + ".jpg",
		LocalPath: path,
		SizeBytes: int64(len(body)),
		SHA256:    hex.EncodeToString(digest[:]),
	}
	if err := f.store.MarkDownloaded(downloaded, at); err != nil {
		t.Fatalf("marking downloaded: %v", err)
	}
	return path
}

func (f *verifyFixture) stateOf(t *testing.T, key string) store.State {
	t.Helper()

	item, err := f.store.Item(key)
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	return item.State
}

func TestVerifyPassesFilesThatAreStillWhatWasDownloaded(t *testing.T) {
	f := newVerifyFixture(t)
	f.backUp(t, "key-a", "photo a bytes")
	f.backUp(t, "key-b", "photo b bytes")

	report, err := Verify(t.Context(), f.store, false)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if report.Checked != 2 || report.Intact != 2 || len(report.Problems) != 0 {
		t.Fatalf("a sound backup verified as %+v", report)
	}
}

// The fault only this sweep can find: same path, same length, different bytes. Nothing else gpb
// does reads a backed-up file again, so silent rot is invisible until someone opens the photograph.
func TestVerifyFindsAFileWhoseBytesChangedUnderIt(t *testing.T) {
	f := newVerifyFixture(t)
	path := f.backUp(t, "key-a", "photo a bytes")
	f.backUp(t, "key-b", "photo b bytes")

	if err := os.WriteFile(path, []byte("rotted a bytes"), 0o644); err != nil {
		t.Fatalf("corrupting the file: %v", err)
	}

	report, err := Verify(t.Context(), f.store, false)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if report.Intact != 1 || len(report.Problems) != 1 {
		t.Fatalf("verification reported %+v, want one corrupt file", report)
	}
	if problem := report.Problems[0]; problem.Fault != FaultCorrupt || problem.Item.MediaKey != "key-a" {
		t.Fatalf("the problem is %s, want key-a corrupt", problem)
	}
	if state := f.stateOf(t, "key-a"); state != store.StateDone {
		t.Fatalf("a sweep without --repair moved key-a to %q", state)
	}
}

func TestVerifyReportsAFileThatIsGone(t *testing.T) {
	f := newVerifyFixture(t)
	path := f.backUp(t, "key-a", "photo a bytes")
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the file: %v", err)
	}

	report, err := Verify(t.Context(), f.store, false)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if len(report.Problems) != 1 || report.Problems[0].Fault != FaultMissing {
		t.Fatalf("verification reported %+v, want one missing file", report)
	}
}

// An item downloaded before there was a hash to record cannot be judged either way, and counting
// it as a fault would report a library full of corruption that is not there.
func TestVerifyCannotJudgeAnItemWithNoRecordedHash(t *testing.T) {
	f := newVerifyFixture(t)
	f.backUp(t, "key-a", "photo a bytes")
	unhashed := store.MediaItem{MediaKey: "key-a", Filename: "key-a.jpg", LocalPath: filepath.Join(f.pool, "key-a.jpg")}
	if err := f.store.MarkDownloaded(unhashed, time.Now()); err != nil {
		t.Fatalf("clearing the recorded hash: %v", err)
	}

	report, err := Verify(t.Context(), f.store, false)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if report.Unhashed != 1 || report.Intact != 0 || len(report.Problems) != 0 {
		t.Fatalf("verification reported %+v, want one unjudgeable file and no fault", report)
	}
}

// An unreadable path is not a lost file. Re-downloading the library because a volume came up late
// would be a far worse outcome than the fault being reported.
func TestVerifyReportsAnUnreadableFileWithoutQueueingIt(t *testing.T) {
	f := newVerifyFixture(t)
	f.backUp(t, "key-a", "photo a bytes")
	path := filepath.Join(f.pool, "key-a.jpg")
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the file: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("putting a directory where the file was: %v", err)
	}

	report, err := Verify(t.Context(), f.store, true)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if len(report.Problems) != 1 || report.Problems[0].Fault != FaultUnreadable {
		t.Fatalf("verification reported %+v, want one unreadable file", report)
	}
	if report.Requeued != 0 {
		t.Fatalf("an unreadable path was queued to be fetched again")
	}
	if state := f.stateOf(t, "key-a"); state != store.StateDone {
		t.Fatalf("an unreadable path moved key-a to %q", state)
	}
}

func TestVerifyRepairPutsBadFilesBackOnTheWorkList(t *testing.T) {
	f := newVerifyFixture(t)
	corrupt := f.backUp(t, "key-a", "photo a bytes")
	gone := f.backUp(t, "key-b", "photo b bytes")
	f.backUp(t, "key-c", "photo c bytes")

	if err := os.WriteFile(corrupt, []byte("rotted a bytes"), 0o644); err != nil {
		t.Fatalf("corrupting the file: %v", err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatalf("removing the file: %v", err)
	}

	report, err := Verify(t.Context(), f.store, true)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if report.Requeued != 2 {
		t.Fatalf("verification queued %d files, want the corrupt one and the missing one", report.Requeued)
	}
	for _, key := range []string{"key-a", "key-b"} {
		if state := f.stateOf(t, key); state != store.StateDiscovered {
			t.Errorf("%s is %q, want %q so the next run fetches it", key, state, store.StateDiscovered)
		}
	}
	if state := f.stateOf(t, "key-c"); state != store.StateDone {
		t.Errorf("the sound file was disturbed: key-c is %q", state)
	}
}
