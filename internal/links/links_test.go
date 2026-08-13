package links

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"gpb/internal/store"
)

type harness struct {
	db        *store.Store
	photosDir string
	t         *testing.T
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return &harness{db: db, photosDir: filepath.Join(root, "photos"), t: t}
}

func (h *harness) album(id, title string, mode store.SyncMode) {
	h.t.Helper()

	if err := h.db.UpsertAlbum(store.Album{ID: id, Title: title}, time.Now()); err != nil {
		h.t.Fatalf("seeding album %s: %v", id, err)
	}
	if err := h.db.SetAlbumSyncMode(id, mode); err != nil {
		h.t.Fatalf("setting the mode of %s: %v", id, err)
	}
}

// item writes a real file into the pool and records it as downloaded, because a link tree
// built from rows that point at nothing would look correct and be useless.
func (h *harness) item(albumID, mediaKey, filename string, capturedAt time.Time) string {
	h.t.Helper()

	poolPath := filepath.Join(h.photosDir, "pool", "2024", "2024-03", mediaKey+"_"+filename)
	if err := os.MkdirAll(filepath.Dir(poolPath), 0o755); err != nil {
		h.t.Fatalf("preparing the pool: %v", err)
	}
	if err := os.WriteFile(poolPath, []byte(mediaKey), 0o644); err != nil {
		h.t.Fatalf("writing a pool file: %v", err)
	}

	now := time.Now()
	if err := h.db.UpsertItem(store.MediaItem{MediaKey: mediaKey, Filename: filename, CapturedAt: capturedAt}, now); err != nil {
		h.t.Fatalf("seeding item %s: %v", mediaKey, err)
	}
	if err := h.db.LinkItemToAlbum(albumID, mediaKey, now); err != nil {
		h.t.Fatalf("linking %s to %s: %v", mediaKey, albumID, err)
	}
	if err := h.db.MarkDownloaded(store.MediaItem{
		MediaKey: mediaKey, Filename: filename, LocalPath: poolPath, SizeBytes: int64(len(mediaKey)),
	}, now); err != nil {
		h.t.Fatalf("marking %s downloaded: %v", mediaKey, err)
	}
	return poolPath
}

func (h *harness) rebuild() Report {
	h.t.Helper()

	report, err := Rebuild(h.db, h.photosDir)
	if err != nil {
		h.t.Fatalf("rebuilding the album view: %v", err)
	}
	return report
}

func (h *harness) names(album string) []string {
	h.t.Helper()

	entries, err := os.ReadDir(filepath.Join(h.photosDir, DirName, album))
	if err != nil {
		h.t.Fatalf("reading album %s: %v", album, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

func TestLinksMirrorFollowedAlbums(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	h.item("holiday", "AF1QipAAAA", "IMG_2041.HEIC", time.Now())
	h.item("holiday", "AF1QipBBBB", "VID_0033.mp4", time.Now())

	h.rebuild()

	if got := h.names("Holiday 2026"); !slices.Equal(got, []string{"IMG_2041.HEIC", "VID_0033.mp4"}) {
		t.Fatalf("the album holds %v", got)
	}
}

// A symlink that does not resolve is worse than no symlink: it looks like a backed-up photo
// until something tries to open it.
func TestLinksResolveToTheRealFile(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	h.item("holiday", "AF1QipAAAA", "IMG_2041.HEIC", time.Now())

	h.rebuild()

	link := filepath.Join(h.photosDir, DirName, "Holiday 2026", "IMG_2041.HEIC")
	content, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("following the link: %v", err)
	}
	if string(content) != "AF1QipAAAA" {
		t.Fatalf("the link points at %q", content)
	}
}

// Absolute targets would name /photos/... inside the container and dangle when the same tree
// is browsed from the host at /srv/gpb/photos. Relative ones survive both.
func TestLinksAreRelativeSoTheTreeCanMove(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	h.item("holiday", "AF1QipAAAA", "IMG_2041.HEIC", time.Now())
	h.rebuild()

	target, err := os.Readlink(filepath.Join(h.photosDir, DirName, "Holiday 2026", "IMG_2041.HEIC"))
	if err != nil {
		t.Fatalf("reading the link: %v", err)
	}
	if filepath.IsAbs(target) {
		t.Fatalf("the link target is absolute: %s", target)
	}

	moved := filepath.Join(t.TempDir(), "relocated")
	if err := os.Rename(h.photosDir, moved); err != nil {
		t.Fatalf("moving the photos tree: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(moved, DirName, "Holiday 2026", "IMG_2041.HEIC")); err != nil {
		t.Fatalf("the link broke when the tree moved: %v", err)
	}
}

// Two phones both producing IMG_2041.HEIC in one album is ordinary. Neither may be dropped.
func TestCollidingFilenamesBothSurvive(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	h.item("holiday", "AF1QipAAAA", "IMG_2041.HEIC", time.Now())
	h.item("holiday", "AF1QipBBBB", "IMG_2041.HEIC", time.Now())

	h.rebuild()

	names := h.names("Holiday 2026")
	if len(names) != 2 {
		t.Fatalf("the album holds %v, want both files", names)
	}
	for _, name := range names {
		if filepath.Ext(name) != ".HEIC" {
			t.Errorf("%q lost its extension to disambiguation", name)
		}
	}
}

func TestUnfollowedAlbumsAreRemoved(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	h.item("holiday", "AF1QipAAAA", "IMG_2041.HEIC", time.Now())
	h.rebuild()

	if err := h.db.SetAlbumSyncMode("holiday", store.SyncNone); err != nil {
		t.Fatalf("unfollowing: %v", err)
	}
	h.rebuild()

	if _, err := os.Stat(filepath.Join(h.photosDir, DirName, "Holiday 2026")); !os.IsNotExist(err) {
		t.Fatalf("the album folder survived an unfollow: %v", err)
	}
}

// The pool is the backup. Pruning a link tree must never be able to reach it.
func TestPruningNeverTouchesThePool(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	poolPath := h.item("holiday", "AF1QipAAAA", "IMG_2041.HEIC", time.Now())
	h.rebuild()

	if err := h.db.SetAlbumSyncMode("holiday", store.SyncNone); err != nil {
		t.Fatalf("unfollowing: %v", err)
	}
	h.rebuild()

	if _, err := os.Stat(poolPath); err != nil {
		t.Fatalf("the pool file went with the links: %v", err)
	}
}

// This tree is disposable; a file someone put here by hand is not.
func TestARealFileBlocksRemovalOfItsFolder(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	h.item("holiday", "AF1QipAAAA", "IMG_2041.HEIC", time.Now())
	h.rebuild()

	notes := filepath.Join(h.photosDir, DirName, "Holiday 2026", "notes.txt")
	if err := os.WriteFile(notes, []byte("mine"), 0o644); err != nil {
		t.Fatalf("writing a hand-made file: %v", err)
	}

	if err := h.db.SetAlbumSyncMode("holiday", store.SyncNone); err != nil {
		t.Fatalf("unfollowing: %v", err)
	}
	h.rebuild()

	if _, err := os.Stat(notes); err != nil {
		t.Fatalf("a hand-made file was deleted: %v", err)
	}
}

// A real file wearing the name a link wanted used to abort the whole rebuild — not that album,
// the lot. Every other album's links then went unwritten because of one file in one folder, and
// the caller logs the failure and carries on, so nothing said why the tree had stopped moving.
func TestARealFileWearingALinkNameCostsOnlyThatLink(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	h.item("holiday", "AF1QipAAAA", "IMG_2041.HEIC", time.Now())
	h.album("garden", "Garden", store.SyncAll)
	h.item("garden", "AF1QipBBBB", "IMG_0001.HEIC", time.Now())
	h.rebuild()

	blocked := filepath.Join(h.photosDir, DirName, "Holiday 2026", "IMG_2041.HEIC")
	if err := os.Remove(blocked); err != nil {
		t.Fatalf("clearing the link: %v", err)
	}
	if err := os.WriteFile(blocked, []byte("mine"), 0o644); err != nil {
		t.Fatalf("writing a hand-made file: %v", err)
	}

	report, err := Rebuild(h.db, h.photosDir)
	if err != nil {
		t.Fatalf("one occupied name failed the whole rebuild: %v", err)
	}
	if report.Albums != 2 {
		t.Errorf("%d albums were rebuilt, want both", report.Albums)
	}

	if got := h.names("Garden"); !slices.Equal(got, []string{"IMG_0001.HEIC"}) {
		t.Errorf("the untouched album holds %v", got)
	}
	kept, err := os.ReadFile(blocked)
	if err != nil || string(kept) != "mine" {
		t.Errorf("the hand-made file is now %q: %v", kept, err)
	}
}

// Rebuild is asked for by the end of a run, by the scheduler and by the CLI. Two at once used
// to fight over the same directories, one removing a link the other had just decided to keep.
func TestConcurrentRebuildsDoNotTreadOnEachOther(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	for _, key := range []string{"AF1QipAAAA", "AF1QipBBBB", "AF1QipCCCC"} {
		h.item("holiday", key, key+".HEIC", time.Now())
	}

	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Rebuild(h.db, h.photosDir); err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)

	for err := range failures {
		t.Errorf("a concurrent rebuild failed: %v", err)
	}
	if got := h.names("Holiday 2026"); len(got) != 3 {
		t.Errorf("the album holds %v, want three links", got)
	}
}

func TestRebuildIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	h.item("holiday", "AF1QipAAAA", "IMG_2041.HEIC", time.Now())

	first := h.rebuild()
	second := h.rebuild()

	if second.Links != first.Links || second.Removed != 0 {
		t.Fatalf("a second rebuild reported %+v, want the same links and no removals", second)
	}
	if got := h.names("Holiday 2026"); len(got) != 1 {
		t.Fatalf("the album holds %v after two rebuilds", got)
	}
}

// An item dropped from an album upstream leaves a link pointing at a file that is no longer
// part of that album. The next rebuild has to notice.
func TestLinksForVanishedItemsAreCleanedUp(t *testing.T) {
	h := newHarness(t)
	h.album("holiday", "Holiday 2026", store.SyncAll)
	h.item("holiday", "AF1QipAAAA", "IMG_2041.HEIC", time.Now())
	h.item("holiday", "AF1QipBBBB", "IMG_2042.HEIC", time.Now())
	h.rebuild()

	if err := h.db.MarkMissingUpstream("AF1QipBBBB", time.Now()); err != nil {
		t.Fatalf("marking an item missing: %v", err)
	}
	report := h.rebuild()

	if got := h.names("Holiday 2026"); !slices.Equal(got, []string{"IMG_2041.HEIC"}) {
		t.Fatalf("the album holds %v, want only the surviving item", got)
	}
	if report.Removed != 1 {
		t.Errorf("the rebuild reported %d removals, want 1", report.Removed)
	}
}

func TestAlbumsSharingATitleGetSeparateFolders(t *testing.T) {
	h := newHarness(t)
	h.album("first", "Wedding", store.SyncAll)
	h.album("second", "Wedding", store.SyncAll)
	h.item("first", "AF1QipAAAA", "IMG_1.HEIC", time.Now())
	h.item("second", "AF1QipBBBB", "IMG_2.HEIC", time.Now())

	report := h.rebuild()
	if report.Albums != 2 {
		t.Fatalf("the rebuild covered %d albums, want 2", report.Albums)
	}

	entries, err := os.ReadDir(filepath.Join(h.photosDir, DirName))
	if err != nil {
		t.Fatalf("reading the album view: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("two albums named Wedding produced %d folders", len(entries))
	}
}

func TestUntitledAlbumsGetAFolderAnyway(t *testing.T) {
	h := newHarness(t)
	h.album("nameless", "", store.SyncAll)
	h.item("nameless", "AF1QipAAAA", "IMG_1.HEIC", time.Now())

	h.rebuild()

	if got := h.names(untitledAlbumDir); !slices.Equal(got, []string{"IMG_1.HEIC"}) {
		t.Fatalf("the untitled album holds %v", got)
	}
}

// An album title is remote input and lands in a path. It must not be able to climb out.
func TestHostileAlbumTitlesCannotEscapeTheTree(t *testing.T) {
	h := newHarness(t)
	h.album("evil", "../../etc", store.SyncAll)
	h.item("evil", "AF1QipAAAA", "IMG_1.HEIC", time.Now())

	h.rebuild()

	entries, err := os.ReadDir(filepath.Join(h.photosDir, DirName))
	if err != nil {
		t.Fatalf("reading the album view: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == ".." || entry.Name() == "." {
			t.Fatalf("a title escaped into %q", entry.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("the hostile title produced %d entries", len(entries))
	}
}
