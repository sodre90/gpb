package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

var noon = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// seedFollowedAlbum gives a test one album the sync engine will visit and one item in it.
func seedFollowedAlbum(t *testing.T, store *Store, mode SyncMode, keys ...string) string {
	t.Helper()

	const albumID = "album-1"
	if err := store.UpsertAlbum(Album{ID: albumID, Title: "Holiday", ItemCount: len(keys)}, noon); err != nil {
		t.Fatalf("seeding the album: %v", err)
	}
	if err := store.SetAlbumSyncMode(albumID, mode); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}

	for _, key := range keys {
		if err := store.UpsertItem(MediaItem{MediaKey: key, Filename: key + ".jpg", CapturedAt: noon}, noon); err != nil {
			t.Fatalf("seeding an item: %v", err)
		}
		if err := store.LinkItemToAlbum(albumID, key, noon); err != nil {
			t.Fatalf("linking an item: %v", err)
		}
	}
	return albumID
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	if err := first.UpsertAlbum(Album{ID: "a", Title: "Kept"}, noon); err != nil {
		t.Fatalf("writing: %v", err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	defer second.Close()

	album, err := second.Album("a")
	if err != nil {
		t.Fatalf("reading the album back: %v", err)
	}
	if album.Title != "Kept" {
		t.Fatalf("the reopened store holds %+v", album)
	}
}

// A nightly listing refreshes what Google reports. If it also reset the user's instruction,
// every followed album would quietly stop syncing after one night.
func TestRelistingAnAlbumKeepsTheUsersSyncMode(t *testing.T) {
	store := openTestStore(t)
	albumID := seedFollowedAlbum(t, store, SyncAll, "item-1")

	later := noon.Add(24 * time.Hour)
	if err := store.UpsertAlbum(Album{ID: albumID, Title: "Holiday renamed", ItemCount: 9}, later); err != nil {
		t.Fatalf("re-listing the album: %v", err)
	}

	followed, err := store.FollowedAlbums()
	if err != nil {
		t.Fatalf("listing followed albums: %v", err)
	}
	if len(followed) != 1 {
		t.Fatalf("the album stopped being followed after a re-listing: %+v", followed)
	}
	if followed[0].Title != "Holiday renamed" || followed[0].ItemCount != 9 {
		t.Errorf("the re-listing did not refresh Google's own fields: %+v", followed[0])
	}
	if !followed[0].FirstSeenAt.Equal(noon) {
		t.Errorf("first_seen_at moved to %s", followed[0].FirstSeenAt)
	}
}

// The same guarantee one level down: re-listing an item must not undo a completed download,
// or a nightly run would re-fetch the entire library.
func TestRelistingAnItemKeepsItsDownloadState(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "item-1")

	done := MediaItem{MediaKey: "item-1", Filename: "IMG_1.jpg", LocalPath: "/pool/IMG_1.jpg",
		SizeBytes: 4096, SHA256: "abc", MimeType: "image/jpeg"}
	if err := store.MarkDownloaded(done, noon); err != nil {
		t.Fatalf("marking downloaded: %v", err)
	}

	later := noon.Add(24 * time.Hour)
	if err := store.UpsertItem(MediaItem{MediaKey: "item-1", Filename: "IMG_1.jpg"}, later); err != nil {
		t.Fatalf("re-listing the item: %v", err)
	}

	item, err := store.Item("item-1")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if item.State != StateDone {
		t.Errorf("state fell back to %q after a re-listing", item.State)
	}
	if item.LocalPath != "/pool/IMG_1.jpg" || item.SizeBytes != 4096 {
		t.Errorf("the re-listing clobbered download fields: %+v", item)
	}
}

// A listing carries no filename, so the syncer passes the media key as a placeholder. The
// real name only ever arrives with the download, and the next nightly listing must not put
// the placeholder back — that would rename every backed-up item to its media key.
func TestRelistingAnItemKeepsItsRealFilename(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "item-1")

	downloaded := MediaItem{MediaKey: "item-1", Filename: "IMG_2041.HEIC", LocalPath: "/pool/x.HEIC"}
	if err := store.MarkDownloaded(downloaded, noon); err != nil {
		t.Fatalf("marking downloaded: %v", err)
	}

	placeholder := MediaItem{MediaKey: "item-1", Filename: "item-1"}
	if err := store.UpsertItem(placeholder, noon.Add(24*time.Hour)); err != nil {
		t.Fatalf("re-listing the item: %v", err)
	}

	item, err := store.Item("item-1")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if item.Filename != "IMG_2041.HEIC" {
		t.Errorf("the re-listing renamed the item to %q", item.Filename)
	}
}

func TestPendingOffersOnlyFollowedWork(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "item-1", "item-2")

	if err := store.UpsertAlbum(Album{ID: "album-2", Title: "Ignored"}, noon); err != nil {
		t.Fatalf("seeding the unfollowed album: %v", err)
	}
	if err := store.UpsertItem(MediaItem{MediaKey: "item-3", Filename: "c.jpg"}, noon); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := store.LinkItemToAlbum("album-2", "item-3", noon); err != nil {
		t.Fatalf("linking: %v", err)
	}

	pending, err := store.Pending(3, 100)
	if err != nil {
		t.Fatalf("querying pending work: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending returned %d items, want the 2 in the followed album", len(pending))
	}
	for _, item := range pending {
		if item.MediaKey == "item-3" {
			t.Error("an item from an unfollowed album was queued for download")
		}
	}
}

func TestPickedAlbumsOnlyOfferSelectedItems(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncPicked, "item-1", "item-2")

	if err := store.SelectItem("item-1", true); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	pending, err := store.Pending(3, 100)
	if err != nil {
		t.Fatalf("querying pending work: %v", err)
	}
	if len(pending) != 1 || pending[0].MediaKey != "item-1" {
		t.Fatalf("a picked album offered %+v, want only the selected item", pending)
	}
}

// An item that fails forever must stop consuming the run's rate budget.
func TestPendingDropsItemsPastTheFailureLimit(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "item-1")

	for range 3 {
		if err := store.MarkFailed("item-1", errors.New("network reset")); err != nil {
			t.Fatalf("marking failed: %v", err)
		}
	}

	pending, err := store.Pending(3, 100)
	if err != nil {
		t.Fatalf("querying pending work: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("an item at the failure limit is still being offered: %+v", pending)
	}

	item, err := store.Item("item-1")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if item.FailCount != 3 || item.LastError != "network reset" {
		t.Errorf("failure bookkeeping is %d/%q", item.FailCount, item.LastError)
	}
}

// A success has to clear the failure history, or an item that failed twice on a flaky night
// would be permanently one failure away from being abandoned.
func TestASuccessfulDownloadClearsTheFailureHistory(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "item-1")

	if err := store.MarkFailed("item-1", errors.New("timeout")); err != nil {
		t.Fatalf("marking failed: %v", err)
	}
	if err := store.MarkDownloaded(MediaItem{MediaKey: "item-1", Filename: "a.jpg", LocalPath: "/pool/a.jpg"}, noon); err != nil {
		t.Fatalf("marking downloaded: %v", err)
	}

	item, err := store.Item("item-1")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if item.FailCount != 0 || item.LastError != "" {
		t.Errorf("the failure history survived a success: %d/%q", item.FailCount, item.LastError)
	}
}

// Google drops an item whose bytes it already holds under another key. Losing one of two
// identical files is not a loss, so that write-off is not put up for review; a copy that
// differs at all — a smaller re-encode of the same shot — still is.
func TestAWrittenOffCopyOfAPhotoStillThereIsNotPutUpForReview(t *testing.T) {
	store := openTestStore(t)
	albumID := seedFollowedAlbum(t, store, SyncAll, "kept", "identical", "re-encoded", "never-fetched")
	for key, sha := range map[string]string{"kept": "abc", "identical": "abc", "re-encoded": "def"} {
		if err := store.MarkDownloaded(MediaItem{MediaKey: key, Filename: key + ".jpg", LocalPath: "/pool/" + key, SHA256: sha}, noon); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}

	later := noon.Add(24 * time.Hour)
	if err := store.LinkItemToAlbum(albumID, "kept", later); err != nil {
		t.Fatalf("re-listing the surviving item: %v", err)
	}
	departed, err := store.ReconcileAlbum(albumID, later)
	if err != nil {
		t.Fatalf("reconciling the album: %v", err)
	}
	if departed != (Departures{LeftTheAlbum: 3, GoneFromGoogle: 3, Copies: 1}) {
		t.Errorf("the reconcile reports %+v, want three gone of which one a copy", departed)
	}

	for key, wantReview := range map[string]bool{"identical": false, "re-encoded": true, "never-fetched": true} {
		item, err := store.Item(key)
		if err != nil {
			t.Fatalf("reading %s: %v", key, err)
		}
		if item.State != StateMissingUpstream || item.NeedsReview != wantReview {
			t.Errorf("%s is %q, review=%v, want missing and review=%v", key, item.State, item.NeedsReview, wantReview)
		}
	}
}

func TestVanishedItemsAreFoundAndNeverDeleted(t *testing.T) {
	store := openTestStore(t)
	albumID := seedFollowedAlbum(t, store, SyncAll, "item-1", "item-2")

	later := noon.Add(24 * time.Hour)
	if err := store.LinkItemToAlbum(albumID, "item-1", later); err != nil {
		t.Fatalf("re-listing the surviving item: %v", err)
	}

	departed, err := store.ReconcileAlbum(albumID, later)
	if err != nil {
		t.Fatalf("reconciling the album: %v", err)
	}
	if departed != (Departures{LeftTheAlbum: 1, GoneFromGoogle: 1}) {
		t.Fatalf("the reconcile reports %+v, want one item leaving and gone", departed)
	}

	item, err := store.Item("item-2")
	if err != nil {
		t.Fatalf("a vanished item was deleted from the store: %v", err)
	}
	if item.State != StateMissingUpstream || !item.NeedsReview {
		t.Errorf("a vanished item is %q, review=%v", item.State, item.NeedsReview)
	}

	// Being written off is what the review queue exists to report, and the queue groups by the
	// album an item was found in. An item nothing else lists has only this album to be grouped
	// under, so its membership is the one thing that must outlive the listing that ended it.
	gone, err := store.ItemsNeedingReview(StateMissingUpstream)
	if err != nil {
		t.Fatalf("reading the review queue: %v", err)
	}
	if len(gone) != 1 || len(gone[0].Items) != 1 || gone[0].Items[0].MediaKey != "item-2" {
		t.Errorf("the review queue holds %+v, want the item that vanished", gone)
	}

	// A second run finds nothing left to reconcile: the membership of everything that merely
	// left is gone rather than merely stale, so the same absence cannot be rediscovered and
	// re-dated every night.
	tomorrow := later.Add(24 * time.Hour)
	if err := store.LinkItemToAlbum(albumID, "item-1", tomorrow); err != nil {
		t.Fatalf("re-listing the surviving item: %v", err)
	}
	again, err := store.ReconcileAlbum(albumID, tomorrow)
	if err != nil {
		t.Fatalf("reconciling a second time: %v", err)
	}
	if again != (Departures{}) {
		t.Errorf("the second reconcile found %+v in an album nothing had left", again)
	}
	unchanged, _ := store.Item("item-2")
	if !unchanged.MissingSince.Equal(later) {
		t.Errorf("missing_since moved to %s on a second run", unchanged.MissingSince)
	}
}

// The pill is a promise that the review page has something to show. Counting anything the page
// leaves out — a flag on an item in an album nobody follows, or one on an item already
// downloaded — is a badge that no amount of reviewing can clear.
func TestThePillCountsOnlyWhatTheReviewPageLists(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "listed", "downloaded")

	if err := store.UpsertAlbum(Album{ID: "declined", Title: "Someone else's", ItemCount: 1}, noon); err != nil {
		t.Fatalf("seeding the unfollowed album: %v", err)
	}
	if err := store.UpsertItem(MediaItem{MediaKey: "unfollowed", Filename: "unfollowed.jpg"}, noon); err != nil {
		t.Fatalf("seeding an item nobody follows: %v", err)
	}
	if err := store.LinkItemToAlbum("declined", "unfollowed", noon); err != nil {
		t.Fatalf("linking an item nobody follows: %v", err)
	}
	if err := store.MarkDownloaded(MediaItem{MediaKey: "downloaded", Filename: "downloaded.jpg",
		LocalPath: "/pool/downloaded.jpg"}, noon); err != nil {
		t.Fatalf("downloading an item: %v", err)
	}
	if err := store.FlagForReview([]string{"listed", "downloaded", "unfollowed"}); err != nil {
		t.Fatalf("flagging the items: %v", err)
	}

	waiting, err := store.CountNeedingReview()
	if err != nil {
		t.Fatalf("counting the review queue: %v", err)
	}
	if listed := countReviewable(t, store); waiting != listed {
		t.Errorf("the pill says %d and the page lists %d", waiting, listed)
	}
	if waiting != 1 {
		t.Errorf("the pill counts %d items, want only the one the page can resolve", waiting)
	}
}

func countReviewable(t *testing.T, store *Store) int {
	t.Helper()

	var listed int
	for _, state := range reviewStates {
		groups, err := store.ItemsNeedingReview(state)
		if err != nil {
			t.Fatalf("reading the %s half of the review queue: %v", state, err)
		}
		for _, group := range groups {
			listed += len(group.Items)
		}
	}
	return listed
}

// Leaving one album is not leaving Google. Recording it as such took the item out of the sync
// set, out of the downloaded set and out of every album's symlinks — for a photo still sitting
// in the library and in whatever other albums hold it.
func TestLeavingOneAlbumIsNotVanishingFromGoogle(t *testing.T) {
	store := openTestStore(t)
	albumID := seedFollowedAlbum(t, store, SyncAll, "item-1", "item-2")

	const otherAlbum = "album-2"
	if err := store.UpsertAlbum(Album{ID: otherAlbum, Title: "Elsewhere"}, noon); err != nil {
		t.Fatalf("seeding the second album: %v", err)
	}
	if err := store.LinkItemToAlbum(otherAlbum, "item-2", noon); err != nil {
		t.Fatalf("linking the item to the second album: %v", err)
	}

	later := noon.Add(24 * time.Hour)
	if err := store.LinkItemToAlbum(albumID, "item-1", later); err != nil {
		t.Fatalf("re-listing the surviving item: %v", err)
	}

	departed, err := store.ReconcileAlbum(albumID, later)
	if err != nil {
		t.Fatalf("reconciling the album: %v", err)
	}
	if departed != (Departures{LeftTheAlbum: 1}) {
		t.Fatalf("the reconcile reports %+v, want one item leaving and none gone", departed)
	}

	item, err := store.Item("item-2")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if item.State == StateMissingUpstream || item.NeedsReview {
		t.Errorf("an item still in another album is %q, review=%v", item.State, item.NeedsReview)
	}

	members, err := store.MembersOf(albumID)
	if err != nil {
		t.Fatalf("reading the membership: %v", err)
	}
	if members["item-2"] {
		t.Error("the item is still a member of the album it left")
	}
}

// An item that comes back has not vanished, and must not keep telling the review queue it has.
func TestAReappearingItemStopsBeingMissing(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "item-1")

	if err := store.MarkMissingUpstream("item-1", noon); err != nil {
		t.Fatalf("marking missing: %v", err)
	}
	if err := store.UpsertItem(MediaItem{MediaKey: "item-1", Filename: "a.jpg"}, noon.Add(time.Hour)); err != nil {
		t.Fatalf("re-listing: %v", err)
	}

	item, err := store.Item("item-1")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if !item.MissingSince.IsZero() {
		t.Errorf("a reappearing item is still missing since %s", item.MissingSince)
	}
}

// Clearing missing_since was not enough on its own. Pending looks for the live states and
// DownloadedInSyncSet looks for done, so an item left sitting at missing_upstream belonged to
// neither set: never fetched again, never verified again, and still asking for a review of a
// disappearance that had been undone.
func TestAReappearingItemIsOfferedForWorkAgain(t *testing.T) {
	reappear := func(t *testing.T, prepare func(*Store)) MediaItem {
		t.Helper()
		store := openTestStore(t)
		seedFollowedAlbum(t, store, SyncAll, "item-1")
		prepare(store)

		if err := store.MarkMissingUpstream("item-1", noon); err != nil {
			t.Fatalf("marking missing: %v", err)
		}
		if err := store.UpsertItem(MediaItem{MediaKey: "item-1"}, noon.Add(time.Hour)); err != nil {
			t.Fatalf("re-listing: %v", err)
		}

		item, err := store.Item("item-1")
		if err != nil {
			t.Fatalf("reading the item: %v", err)
		}
		if item.NeedsReview {
			t.Error("a reappearing item still asks to be reviewed as missing")
		}
		return item
	}

	t.Run("one that was never downloaded is queued again", func(t *testing.T) {
		item := reappear(t, func(*Store) {})
		if item.State != StateDiscovered {
			t.Fatalf("a reappearing item is %q, want %q", item.State, StateDiscovered)
		}
	})

	t.Run("one that is on disk goes back to being on disk", func(t *testing.T) {
		item := reappear(t, func(store *Store) {
			done := MediaItem{MediaKey: "item-1", LocalPath: "2026/08/item-1.jpg", SizeBytes: 12}
			if err := store.MarkDownloaded(done, noon); err != nil {
				t.Fatalf("marking downloaded: %v", err)
			}
		})
		if item.State != StateDone {
			t.Fatalf("a reappearing item that is on disk is %q, want %q", item.State, StateDone)
		}
	})
}

// SQL compares stored timestamps as strings, so a format whose fractional part varies in
// width would order them wrongly and hide vanished items. This is the sub-second case that
// time.RFC3339Nano gets wrong.
func TestStoredTimestampsSortChronologically(t *testing.T) {
	store := openTestStore(t)
	albumID := seedFollowedAlbum(t, store, SyncAll, "item-1", "item-2")

	listedAt := noon.Add(500 * time.Millisecond)
	if err := store.LinkItemToAlbum(albumID, "item-1", listedAt); err != nil {
		t.Fatalf("re-listing the surviving item: %v", err)
	}

	departed, err := store.ReconcileAlbum(albumID, listedAt)
	if err != nil {
		t.Fatalf("reconciling the album: %v", err)
	}
	if departed.LeftTheAlbum != 1 {
		t.Fatalf("%d items left the album, want 1 — timestamps are not sorting",
			departed.LeftTheAlbum)
	}
}

func TestSyncRunsRecordTheirOutcome(t *testing.T) {
	store := openTestStore(t)

	id, err := store.StartRun(noon)
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}

	unfinished, err := store.RecentRuns(10)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if !unfinished[0].FinishedAt.IsZero() {
		t.Error("a run reported a finish time before it finished")
	}

	err = store.FinishRun(SyncRun{ID: id, Outcome: OutcomePartial, Listed: 10,
		Downloaded: 8, Failed: 2, Bytes: 4096, Error: "two timeouts"}, noon.Add(time.Minute))
	if err != nil {
		t.Fatalf("finishing the run: %v", err)
	}

	runs, err := store.RecentRuns(10)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if runs[0].Outcome != OutcomePartial || runs[0].Downloaded != 8 || runs[0].Bytes != 4096 {
		t.Errorf("the run recorded %+v", runs[0])
	}
	if runs[0].FinishedAt.IsZero() {
		t.Error("a finished run has no finish time")
	}
}

func TestUnknownSyncModesAreRefused(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "item-1")

	if err := store.SetAlbumSyncMode("album-1", SyncMode("everything")); err == nil {
		t.Error("an unknown sync mode was accepted")
	}
	if err := store.SetAlbumSyncMode("no-such-album", SyncAll); err == nil {
		t.Error("setting the mode on a missing album was accepted")
	}
}

func TestCountsSummariseTheLibrary(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "item-1", "item-2")

	if err := store.MarkDownloaded(MediaItem{MediaKey: "item-1", Filename: "a.jpg", LocalPath: "/pool/a.jpg"}, noon); err != nil {
		t.Fatalf("marking downloaded: %v", err)
	}

	counts, err := store.Counts()
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if counts[StateDone] != 1 || counts[StateDiscovered] != 1 {
		t.Errorf("counts are %v", counts)
	}
}

// A photo in three followed albums is one photo and one download. The summary is built from the
// per-item table for exactly this reason: adding up the per-album counts would promise the user
// three times the work, and then report a third of it done when it finished.
func TestTheBackupSetCountsAPhotoOnceHoweverManyAlbumsHoldIt(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "shared-key")

	for _, albumID := range []string{"album-2", "album-3"} {
		if err := store.UpsertAlbum(Album{ID: albumID, Title: albumID, ItemCount: 1}, noon); err != nil {
			t.Fatalf("seeding %s: %v", albumID, err)
		}
		if err := store.SetAlbumSyncMode(albumID, SyncAll); err != nil {
			t.Fatalf("following %s: %v", albumID, err)
		}
		if err := store.LinkItemToAlbum(albumID, "shared-key", noon); err != nil {
			t.Fatalf("linking into %s: %v", albumID, err)
		}
	}

	set, err := store.BackupSet()
	if err != nil {
		t.Fatalf("summarising the backup set: %v", err)
	}
	if set.Albums != 3 {
		t.Errorf("the set covers %d albums, want 3", set.Albums)
	}
	if set.Known != 1 {
		t.Errorf("the set holds %d photos, want 1 — the same photo three times over", set.Known)
	}
}

// An album nobody asked for must not appear in the totals, and neither must its items: the whole
// promise of the summary is that it describes what a run would actually fetch.
func TestTheBackupSetIgnoresAlbumsNobodyFollows(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncNone, "unwanted-key")

	set, err := store.BackupSet()
	if err != nil {
		t.Fatalf("summarising the backup set: %v", err)
	}
	if !set.Empty() {
		t.Errorf("an unfollowed album counts as %+v, want an empty set", set)
	}
	if set.Known != 0 || set.Expected != 0 {
		t.Errorf("the set promises %d photos (%d expected) from an album nobody follows", set.Known, set.Expected)
	}
}

// A 'picked' album contributes only what was picked. Counting all of its contents would tell the
// user a curated album of six photos is a backup of six hundred.
func TestTheBackupSetCountsOnlyThePicksOfAPickedAlbum(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncPicked, "wanted-key", "passed-over-key")

	if err := store.SelectItem("wanted-key", true); err != nil {
		t.Fatalf("picking an item: %v", err)
	}

	set, err := store.BackupSet()
	if err != nil {
		t.Fatalf("summarising the backup set: %v", err)
	}
	if set.Known != 1 {
		t.Errorf("the set holds %d photos, want only the one that was picked", set.Known)
	}
}

// The one trap in UpsertAlbum's shape: it takes an Album, so a caller can fill in SyncMode and
// reasonably expect it to apply. It never does, deliberately — but nothing said so until this
// test did, and the local UI preview was seeded wrong for exactly this reason.
func TestUpsertAlbumIgnoresTheSyncModeItIsHanded(t *testing.T) {
	store := openTestStore(t)

	if err := store.UpsertAlbum(Album{ID: "a", Title: "Holiday", SyncMode: SyncAll}, noon); err != nil {
		t.Fatalf("inserting the album: %v", err)
	}
	album, err := store.Album("a")
	if err != nil {
		t.Fatalf("reading the album: %v", err)
	}
	if album.SyncMode != SyncNone {
		t.Errorf("a listing set sync_mode to %q; only the user may", album.SyncMode)
	}

	if err := store.SetAlbumSyncMode("a", SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	if err := store.UpsertAlbum(Album{ID: "a", Title: "Holiday", SyncMode: SyncAll}, noon); err != nil {
		t.Fatalf("re-listing the album: %v", err)
	}
	album, err = store.Album("a")
	if err != nil {
		t.Fatalf("re-reading the album: %v", err)
	}
	if album.SyncMode != SyncPicked {
		t.Errorf("a re-listing changed sync_mode to %q; the user chose picked", album.SyncMode)
	}
}

// The rows that made this migration necessary are on the box in their tens of thousands: every
// item listed but not yet downloaded, each holding its media key where its name belongs.
func TestItemsNamedAfterTheirMediaKeyAreLeftWithNoNameAtAll(t *testing.T) {
	store := openTestStore(t)

	placeholder := MediaItem{MediaKey: "key-a", Filename: "key-a"}
	if err := store.UpsertItem(placeholder, noon); err != nil {
		t.Fatalf("seeding the placeholder: %v", err)
	}
	named := MediaItem{MediaKey: "key-b", Filename: "IMG_0001.HEIC"}
	if err := store.UpsertItem(named, noon); err != nil {
		t.Fatalf("seeding the named item: %v", err)
	}

	blank, err := migrations.ReadFile("migrations/0009_placeholder_filenames.sql")
	if err != nil {
		t.Fatalf("reading the migration: %v", err)
	}
	if _, err := store.db.Exec(string(blank)); err != nil {
		t.Fatalf("applying the blanking: %v", err)
	}

	for _, want := range []MediaItem{{MediaKey: "key-a", Filename: ""}, {MediaKey: "key-b", Filename: "IMG_0001.HEIC"}} {
		item, err := store.Item(want.MediaKey)
		if err != nil {
			t.Fatalf("reading %s back: %v", want.MediaKey, err)
		}
		if item.Filename != want.Filename {
			t.Errorf("%s is named %q, want %q", want.MediaKey, item.Filename, want.Filename)
		}
	}
}

// The rows that made this migration necessary are already on the box: runs a restart cut off,
// recorded as failures, which is what the schedule reads when it asks if a backup is owed.
func TestRunsCutOffByARestartAreReclassifiedRatherThanLeftAsFailures(t *testing.T) {
	store := openTestStore(t)

	cancelled, err := store.StartRun(time.Now().Add(-2 * time.Hour))
	if err != nil {
		t.Fatalf("recording the cancelled run: %v", err)
	}
	if err := store.FinishRun(SyncRun{ID: cancelled, Outcome: OutcomeError, Error: "context canceled"}, time.Now()); err != nil {
		t.Fatalf("finishing the cancelled run: %v", err)
	}

	broken, err := store.StartRun(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("recording the failed run: %v", err)
	}
	if err := store.FinishRun(SyncRun{ID: broken, Outcome: OutcomeError, Error: "google refused the listing"}, time.Now()); err != nil {
		t.Fatalf("finishing the failed run: %v", err)
	}

	// The migration itself rather than a copy of it: a fresh store has already run it, and the
	// rows it exists for only appear afterwards in a test.
	reclassify, err := migrations.ReadFile("migrations/0008_interrupted_runs.sql")
	if err != nil {
		t.Fatalf("reading the migration: %v", err)
	}
	if _, err := store.db.Exec(string(reclassify)); err != nil {
		t.Fatalf("applying the reclassification: %v", err)
	}

	runs, err := store.RecentRuns(2)
	if err != nil {
		t.Fatalf("reading the runs back: %v", err)
	}
	if runs[0].Outcome != OutcomeError {
		t.Errorf("a run that failed on its own was reclassified as %q", runs[0].Outcome)
	}
	if runs[1].Outcome != OutcomeInterrupted {
		t.Errorf("a run cut off by a restart is still recorded as %q", runs[1].Outcome)
	}
}

// The renewal is a single statement on purpose: a session that expires between an "is it live"
// query and the write that slides it would be renewed after its own death.
func TestOnlyALiveSessionIsRenewed(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()

	if err := store.StartWebSession("abc123", now.Add(-time.Hour)); err != nil {
		t.Fatalf("starting a session: %v", err)
	}

	renewed, err := store.RenewWebSession("abc123", now, now.Add(-30*time.Minute), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("renewing an idle session: %v", err)
	}
	if renewed {
		t.Error("a session idle for an hour was renewed under a 30-minute window")
	}

	renewed, err = store.RenewWebSession("abc123", now, now.Add(-2*time.Hour), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("renewing a live session: %v", err)
	}
	if !renewed {
		t.Error("a session inside its idle window was not renewed")
	}
}

// The absolute limit is what a cookie kept warm cannot outlive, so it has to be checked against
// when the session started rather than when it was last used.
func TestASessionKeptWarmStillExpiresAtItsAbsoluteLimit(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()

	if err := store.StartWebSession("kept-warm", now.Add(-100*24*time.Hour)); err != nil {
		t.Fatalf("starting a session: %v", err)
	}
	if _, err := store.RenewWebSession("kept-warm", now, now.Add(-time.Hour), time.Time{}); err != nil {
		t.Fatalf("renewing it: %v", err)
	}

	live, err := store.WebSessionIsLive("kept-warm", now.Add(-time.Hour), now.Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if live {
		t.Error("a session started 100 days ago is live under a 90-day absolute limit")
	}
}

func TestEndingASessionRemovesIt(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()

	if err := store.StartWebSession("going", now); err != nil {
		t.Fatalf("starting a session: %v", err)
	}
	if err := store.EndWebSession("going"); err != nil {
		t.Fatalf("ending it: %v", err)
	}

	live, err := store.WebSessionIsLive("going", now.Add(-time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if live {
		t.Error("a session survived being logged out")
	}
}

// seedAlbumOfItems gives a test an album under full backup with items of its own, so two of them
// can be compared for which the run reaches first.
func seedAlbumOfItems(t *testing.T, store *Store, albumID, title string, capturedAt time.Time, keys ...string) {
	t.Helper()

	if err := store.UpsertAlbum(Album{ID: albumID, Title: title, ItemCount: len(keys)}, noon); err != nil {
		t.Fatalf("seeding %s: %v", albumID, err)
	}
	if err := store.SetAlbumSyncMode(albumID, SyncAll); err != nil {
		t.Fatalf("following %s: %v", albumID, err)
	}
	for _, key := range keys {
		if err := store.UpsertItem(MediaItem{MediaKey: key, Filename: key + ".jpg", CapturedAt: capturedAt}, noon); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
		if err := store.LinkItemToAlbum(albumID, key, noon); err != nil {
			t.Fatalf("linking %s: %v", key, err)
		}
	}
}

// A refresh writes every album column from Google's listing. This one is not Google's to write:
// it is the only thing on the row the user put there.
func TestARefreshDoesNotClearAFavourite(t *testing.T) {
	store := openTestStore(t)

	if err := store.UpsertAlbum(Album{ID: "a", Title: "Holiday"}, noon); err != nil {
		t.Fatalf("inserting the album: %v", err)
	}
	if err := store.SetAlbumFavourite("a", true); err != nil {
		t.Fatalf("starring it: %v", err)
	}
	if err := store.UpsertAlbum(Album{ID: "a", Title: "Holiday"}, noon.Add(time.Hour)); err != nil {
		t.Fatalf("re-listing the album: %v", err)
	}

	album, err := store.Album("a")
	if err != nil {
		t.Fatalf("reading the album: %v", err)
	}
	if !album.Favourite {
		t.Error("a nightly refresh unstarred an album the user had starred")
	}

	if err := store.SetAlbumFavourite("a", false); err != nil {
		t.Fatalf("unstarring it: %v", err)
	}
	if album, err = store.Album("a"); err != nil || album.Favourite {
		t.Errorf("unstarring left the album starred (%v)", err)
	}
}

// A walk of 181 albums against a service asked twice a second is long enough to be cut off, so
// the order it goes in decides what a cut-off run got through.
func TestAWalkVisitsFavouriteAlbumsFirst(t *testing.T) {
	store := openTestStore(t)
	for _, title := range []string{"Anna", "Berlin", "Zermatt"} {
		seedAlbumOfItems(t, store, "album-"+title, title, noon)
	}
	if err := store.SetAlbumFavourite("album-Zermatt", true); err != nil {
		t.Fatalf("starring: %v", err)
	}

	followed, err := store.FollowedAlbums()
	if err != nil {
		t.Fatalf("listing what a run walks: %v", err)
	}
	if len(followed) != 3 || followed[0].Title != "Zermatt" {
		t.Fatalf("a run walks %v, want the starred Zermatt first", titlesOf(followed))
	}
	// The rest keep their own order, or starring one album would rearrange the other 180.
	if followed[1].Title != "Anna" || followed[2].Title != "Berlin" {
		t.Errorf("the albums behind the favourite are %v, want them still in title order",
			titlesOf(followed[1:]))
	}
}

func titlesOf(albums []Album) []string {
	titles := make([]string, 0, len(albums))
	for _, album := range albums {
		titles = append(titles, album.Title)
	}
	return titles
}

// The download queue is the other half of the same promise: walking a favourite first buys
// nothing if its photographs then queue behind 90,000 older ones.
func TestFavouriteAlbumsAreDownloadedFirst(t *testing.T) {
	store := openTestStore(t)
	// The favourite holds the older photographs, so capture date alone would put it last.
	seedAlbumOfItems(t, store, "album-old", "Old", noon.Add(-24*time.Hour), "old-1", "old-2")
	seedAlbumOfItems(t, store, "album-new", "New", noon, "new-1", "new-2")
	if err := store.SetAlbumFavourite("album-old", true); err != nil {
		t.Fatalf("starring: %v", err)
	}

	pending, err := store.Pending(3, 100)
	if err != nil {
		t.Fatalf("querying pending work: %v", err)
	}
	if len(pending) != 4 {
		t.Fatalf("pending returned %d items, want all 4: this is an ordering, not a filter", len(pending))
	}
	for _, item := range pending[:2] {
		if !strings.HasPrefix(item.MediaKey, "old-") {
			t.Errorf("the queue starts %v, want the starred album's items first", keysOf(pending))
			break
		}
	}
}

func keysOf(items []MediaItem) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.MediaKey)
	}
	return keys
}

// Verification asks a different question from a sync run, so it reads a different set: a file
// backed up before its album was unfollowed is out of the sync set but still on the disk, and
// still one this backup claims is intact.
func TestDownloadedFilesReachesOutsideTheSyncSet(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "kept")
	if err := store.UpsertItem(MediaItem{MediaKey: "unfollowed", Filename: "b.jpg", CapturedAt: noon}, noon); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	for _, key := range []string{"kept", "unfollowed"} {
		if err := store.MarkDownloaded(MediaItem{MediaKey: key, LocalPath: key + ".jpg", SizeBytes: 1}, noon); err != nil {
			t.Fatalf("marking downloaded: %v", err)
		}
	}

	inSyncSet, err := store.DownloadedInSyncSet()
	if err != nil {
		t.Fatalf("querying the sync set: %v", err)
	}
	if len(inSyncSet) != 1 {
		t.Fatalf("the sync set holds %v, want only the followed item", keysOf(inSyncSet))
	}

	onDisk, err := store.DownloadedFiles()
	if err != nil {
		t.Fatalf("querying the downloaded files: %v", err)
	}
	if len(onDisk) != 2 {
		t.Fatalf("the downloaded files are %v, want both", keysOf(onDisk))
	}
}
