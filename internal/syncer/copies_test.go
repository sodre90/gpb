package syncer

import (
	"os"
	"testing"
	"time"

	"gpb/internal/gphotos"
	"gpb/internal/store"
)

func pathOf(t *testing.T, db *store.Store, key string) string {
	t.Helper()
	item, err := db.Item(key)
	if err != nil {
		t.Fatalf("reading %s: %v", key, err)
	}
	if item.State != store.StateDone || item.LocalPath == "" {
		t.Fatalf("%s is %s with no file", key, item.State)
	}
	return item.LocalPath
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	first, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	second, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(first, second)
}

// Google names one photograph differently in an album and on the timeline, and a library
// followed alongside its albums used to keep it twice — 626 GB of one pool.
func TestADownloadThatRepeatsAHeldPhotoBecomesALinkToIt(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"album-key": "the same photo", "timeline-key": "the same photo"})
	h.syncer.options.Workers = 1

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	first, second := pathOf(t, h.store, "album-key"), pathOf(t, h.store, "timeline-key")
	if first == second {
		t.Fatal("both keys recorded the same path; each key needs a name of its own")
	}
	if !sameFile(t, first, second) {
		t.Error("the repeated photo was kept as a second file")
	}
	if leftovers, _ := os.ReadDir(h.temp); len(leftovers) != 0 {
		t.Errorf("the staging directory kept %d files after a download committed as a link", len(leftovers))
	}
}

// Linking to a copy that has rotted would throw away the good bytes just downloaded and leave
// two names for the bad ones.
func TestARottedHeldCopyIsNotLinkedTo(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"album-key": "the same photo"})
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	held := pathOf(t, h.store, "album-key")
	if err := os.WriteFile(held, []byte("the same phot0"), 0o644); err != nil {
		t.Fatalf("rotting the held copy: %v", err)
	}

	h.source.items["album-1"] = append(h.source.items["album-1"], gphotos.MediaItem{
		MediaKey: "timeline-key", CapturedAt: time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC), Width: 100, Height: 100,
	})
	h.source.bodies["timeline-key"] = []byte("the same photo")
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the second run: %v", err)
	}

	fresh := pathOf(t, h.store, "timeline-key")
	if sameFile(t, held, fresh) {
		t.Fatal("the fresh download was replaced by a link to a rotted copy")
	}
	if body, _ := os.ReadFile(fresh); string(body) != "the same photo" {
		t.Errorf("the fresh download holds %q", body)
	}
}

func TestLinkCopiesCountsThenLinksWhatIsAlreadyOnDisk(t *testing.T) {
	f := newVerifyFixture(t)
	first := f.backUp(t, "album-key", "the same photo")
	second := f.backUp(t, "timeline-key", "the same photo")
	f.backUp(t, "another-photo", "a different photo")

	counted, err := LinkCopies(t.Context(), f.store, false)
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if counted.Photos != 1 || counted.Separate != 1 || counted.Linked != 0 {
		t.Fatalf("counted %+v, want one photo with one extra file and nothing linked", counted)
	}
	if sameFile(t, first, second) {
		t.Fatal("counting linked the files")
	}

	linked, err := LinkCopies(t.Context(), f.store, true)
	if err != nil {
		t.Fatalf("linking: %v", err)
	}
	if linked.Linked != 1 || linked.Freed != int64(len("the same photo")) || len(linked.Refusals) != 0 {
		t.Fatalf("linking did %+v", linked)
	}
	if !sameFile(t, first, second) {
		t.Fatal("the two copies are still two files")
	}

	again, err := LinkCopies(t.Context(), f.store, true)
	if err != nil {
		t.Fatalf("linking again: %v", err)
	}
	if again.Separate != 0 || again.Linked != 0 {
		t.Errorf("a second pass found work to do: %+v", again)
	}
}

func TestLinkCopiesLeavesACopyThatNoLongerMatchesItsRecord(t *testing.T) {
	f := newVerifyFixture(t)
	first := f.backUp(t, "album-key", "the same photo")
	second := f.backUp(t, "timeline-key", "the same photo")
	if err := os.WriteFile(second, []byte("the same phot0"), 0o644); err != nil {
		t.Fatalf("rotting a copy: %v", err)
	}

	linking, err := LinkCopies(t.Context(), f.store, true)
	if err != nil {
		t.Fatalf("linking: %v", err)
	}
	if linking.Linked != 0 || len(linking.Refusals) != 1 {
		t.Fatalf("linking did %+v, want the rotted copy refused", linking)
	}
	if sameFile(t, first, second) {
		t.Error("a copy that failed verification was linked over")
	}
}

// Two names for one file are one read: the second name is answered from the first.
func TestVerifyReadsAFileWithTwoNamesOnce(t *testing.T) {
	f := newVerifyFixture(t)
	first := f.backUp(t, "album-key", "the same photo")
	second := f.backUp(t, "timeline-key", "the same photo")
	if err := linkOver(first, second); err != nil {
		t.Fatalf("linking: %v", err)
	}

	digests := digestsByFile{}
	want, err := digests.hash(first)
	if err != nil {
		t.Fatalf("hashing the first name: %v", err)
	}
	if err := os.Chmod(second, 0o000); err != nil {
		t.Fatalf("making the file unreadable: %v", err)
	}
	t.Cleanup(func() { os.Chmod(second, 0o644) })

	got, err := digests.hash(second)
	if err != nil {
		t.Fatalf("the second name was read again rather than answered from the first: %v", err)
	}
	if got != want {
		t.Errorf("the second name hashed to %s, want %s", got, want)
	}

	if err := os.Chmod(second, 0o644); err != nil {
		t.Fatalf("making the file readable again: %v", err)
	}
	report, err := Verify(t.Context(), f.store, false)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if report.Checked != 2 || report.Intact != 2 {
		t.Errorf("verify reported %s, want both names checked and intact", report)
	}
}

// A written-off copy that had been linked to the photo that stays is one name for the kept
// file; removing it has to leave the kept photo exactly as it was.
func TestRemovingALinkedWrittenOffCopyLeavesTheKeptPhoto(t *testing.T) {
	f := newVerifyFixture(t)
	kept := f.backUp(t, "kept", "the same photo")
	twin := f.backUp(t, "twin", "the same photo")
	if err := linkOver(kept, twin); err != nil {
		t.Fatalf("linking: %v", err)
	}
	if err := f.store.MarkMissingUpstream("twin", time.Now()); err != nil {
		t.Fatalf("writing the twin off: %v", err)
	}

	cleanup, err := RemoveTwins(t.Context(), f.store, true)
	if err != nil {
		t.Fatalf("removing twins: %v", err)
	}
	if cleanup.Removed != 1 {
		t.Fatalf("removed %d, want the linked twin", cleanup.Removed)
	}
	if body, err := os.ReadFile(kept); err != nil || string(body) != "the same photo" {
		t.Errorf("the kept photo reads %q, %v after its linked twin was removed", body, err)
	}
}
