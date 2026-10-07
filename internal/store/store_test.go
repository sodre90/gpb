package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
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
		if err := store.LinkItemToAlbum(albumID, key); err != nil {
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
		SizeBytes: 4096, SHA256: "abc"}
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
	if err := store.LinkItemToAlbum("album-2", "item-3"); err != nil {
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

// Google drops an item it already holds under another key. Losing one of two copies is not a
// loss, so that write-off is not put up for review: the same bytes, or the same file name and
// capture second with the larger copy staying. The larger copy going is still a question.
func TestAWrittenOffCopyOfAPhotoStillThereIsNotPutUpForReview(t *testing.T) {
	store := openTestStore(t)
	albumID := seedFollowedAlbum(t, store, SyncAll, "kept", "identical", "re-encoded", "better-than-kept", "never-fetched")
	downloaded := map[string]MediaItem{
		"kept":             {Filename: "IMAG0003.jpg", SHA256: "abc", SizeBytes: 1000},
		"identical":        {Filename: "copy.jpg", SHA256: "abc", SizeBytes: 1000},
		"re-encoded":       {Filename: "IMAG0003.jpg", SHA256: "def", SizeBytes: 700},
		"better-than-kept": {Filename: "IMAG0003.jpg", SHA256: "ghi", SizeBytes: 1300},
	}
	for key, item := range downloaded {
		item.MediaKey, item.LocalPath = key, "/pool/"+key
		if err := store.MarkDownloaded(item, noon); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}

	later := noon.Add(24 * time.Hour)
	if err := store.LinkItemToAlbum(albumID, "kept"); err != nil {
		t.Fatalf("re-listing the surviving item: %v", err)
	}
	departed, err := store.Reconcile(albumID, []string{"identical", "re-encoded", "better-than-kept", "never-fetched"}, later)
	if err != nil {
		t.Fatalf("reconciling the album: %v", err)
	}
	if departed != (Departures{LeftTheAlbum: 4, GoneFromGoogle: 4, Copies: 2}) {
		t.Errorf("the reconcile reports %+v, want four gone of which two copies", departed)
	}

	for key, wantReview := range map[string]bool{"identical": false, "re-encoded": false, "better-than-kept": true, "never-fetched": true} {
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
	if err := store.LinkItemToAlbum(albumID, "item-1"); err != nil {
		t.Fatalf("re-listing the surviving item: %v", err)
	}

	departed, err := store.Reconcile(albumID, []string{"item-2"}, later)
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
	if err := store.LinkItemToAlbum(albumID, "item-1"); err != nil {
		t.Fatalf("re-listing the surviving item: %v", err)
	}
	again, err := store.Reconcile(albumID, []string{"item-2"}, tomorrow)
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
	if err := store.LinkItemToAlbum("declined", "unfollowed"); err != nil {
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
	if err := store.LinkItemToAlbum(otherAlbum, "item-2"); err != nil {
		t.Fatalf("linking the item to the second album: %v", err)
	}

	later := noon.Add(24 * time.Hour)
	if err := store.LinkItemToAlbum(albumID, "item-1"); err != nil {
		t.Fatalf("re-listing the surviving item: %v", err)
	}

	departed, err := store.Reconcile(albumID, []string{"item-2"}, later)
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

	members, err := store.AlbumMembers(albumID)
	if err != nil {
		t.Fatalf("reading the membership: %v", err)
	}
	if _, still := members["item-2"]; still {
		t.Error("the item is still a member of the album it left")
	}
}

// relistedUnchanged is item-1 described exactly as seedFollowedAlbum stored it, so a re-listing of
// it changes nothing Google reports and only the item's own state can let the update through.
var relistedUnchanged = MediaItem{MediaKey: "item-1", Filename: "item-1.jpg", CapturedAt: noon}

// An item that comes back has not vanished, and must not keep telling the review queue it has.
func TestAReappearingItemStopsBeingMissing(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "item-1")

	if err := store.MarkMissingUpstream("item-1", noon); err != nil {
		t.Fatalf("marking missing: %v", err)
	}
	if err := store.UpsertItem(relistedUnchanged, noon.Add(time.Hour)); err != nil {
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
		if err := store.UpsertItem(relistedUnchanged, noon.Add(time.Hour)); err != nil {
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

// Up to 0.4.2 a download that finished while the same run wrote its item off left the item done
// with a missing_since still on it, and Reconcile reads any missing_since as "already written
// off". The download path no longer leaves that behind, but databases still hold the rows it did,
// so the row is written here as that release wrote it. A re-listing has to clear the date even
// though nothing Google reports has changed, or the photo's real disappearance later takes it out
// of its album without ever reaching the review queue.
func TestARelistingClearsAMissingDateAnOlderReleaseLeftBehind(t *testing.T) {
	store := openTestStore(t)
	albumID := seedFollowedAlbum(t, store, SyncAll, "item-1")

	if _, err := store.db.Exec(`
		UPDATE media_items SET state = ?, local_path = '2026/08/item-1.jpg', missing_since = ?, needs_review = 1
		WHERE media_key = 'item-1'`, string(StateDone), formatTime(noon)); err != nil {
		t.Fatalf("writing the row 0.4.2 left: %v", err)
	}
	if err := store.UpsertItem(relistedUnchanged, noon.Add(time.Hour)); err != nil {
		t.Fatalf("re-listing: %v", err)
	}

	departed, err := store.Reconcile(albumID, []string{"item-1"}, noon.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("reconciling the photo's real disappearance: %v", err)
	}
	if departed.GoneFromGoogle != 1 {
		t.Errorf("a downloaded photo that later left Google reconciled as %+v, want it written off", departed)
	}
}

// Listing and downloading run side by side, so a walk can write an item off while a worker holds
// it. Whatever the worker then reports is older news than the write-off, and must not undo it:
// the item stays missing, keeps the date it was noticed gone, and a second walk over the same
// absence finds nothing left to do. A file that did arrive is still recorded.
func TestTheDownloadPathLeavesAWriteOffStanding(t *testing.T) {
	landed := MediaItem{MediaKey: "item-1", Filename: "IMG_0001.jpg", LocalPath: "2026/08/item-1.jpg", SizeBytes: 12}
	for name, test := range map[string]struct {
		report   func(*Store) error
		wantPath string
	}{
		"a download that finished": {func(store *Store) error { return store.MarkDownloaded(landed, noon) }, landed.LocalPath},
		"a download that failed": {func(store *Store) error {
			return store.MarkFailed("item-1", errors.New("the content host timed out"))
		}, ""},
		"a worker taking the item": {func(store *Store) error { return store.SetItemState("item-1", StateDownloading) }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t)
			albumID := seedFollowedAlbum(t, store, SyncAll, "item-1")
			if _, err := store.Reconcile(albumID, []string{"item-1"}, noon); err != nil {
				t.Fatalf("writing the item off: %v", err)
			}

			if err := test.report(store); err != nil {
				t.Fatalf("reporting back: %v", err)
			}

			item, err := store.Item("item-1")
			if err != nil {
				t.Fatalf("reading the item: %v", err)
			}
			if item.State != StateMissingUpstream || !item.MissingSince.Equal(noon) {
				t.Errorf("the write-off came out as %s, missing since %s; want it still missing since noon",
					item.State, item.MissingSince)
			}
			if item.LocalPath != test.wantPath {
				t.Errorf("the item's file is recorded as %q, want %q", item.LocalPath, test.wantPath)
			}
			again, err := store.Reconcile(albumID, []string{"item-1"}, noon.Add(24*time.Hour))
			if err != nil {
				t.Fatalf("reconciling the same absence again: %v", err)
			}
			if again != (Departures{}) {
				t.Errorf("a second walk over the same absence reconciled %+v, want nothing", again)
			}
		})
	}
}

// SQL compares stored timestamps as strings, so a format whose fractional part varies in width
// would order them wrongly: time.RFC3339Nano writes noon as "12:00:00Z", which sorts after
// "12:00:00.5Z". The last moment is an hour later on a clock five hours behind, which written in
// its own zone would read 08:00 and sort first.
func TestStoredTimestampsSortChronologically(t *testing.T) {
	moments := []time.Time{
		noon,
		noon.Add(time.Nanosecond),
		noon.Add(500 * time.Millisecond),
		noon.Add(time.Second),
		noon.Add(time.Second + time.Nanosecond),
		noon.Add(time.Hour).In(time.FixedZone("UTC-5", -5*60*60)),
	}

	var stored []string
	for _, moment := range moments {
		stored = append(stored, formatTime(moment))
	}
	if !slices.IsSorted(stored) {
		t.Errorf("chronologically ordered moments stored out of order: %q", stored)
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
		if err := store.LinkItemToAlbum(albumID, "shared-key"); err != nil {
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

// Two keys for one photograph are two photos backed up and one file on disk: the pool keeps them
// as hardlinks, and the overview shows this figure as what is on disk.
func TestTheBackupSetMeasuresAPhotoHeldUnderTwoKeysOnce(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "album-key", "timeline-key", "other-key")
	for key, sha := range map[string]string{"album-key": "same", "timeline-key": "same", "other-key": "other"} {
		item := MediaItem{MediaKey: key, Filename: key + ".jpg", LocalPath: "/pool/" + key + ".jpg", SizeBytes: 1000, SHA256: sha}
		if err := store.MarkDownloaded(item, noon); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}

	set, err := store.BackupSet()
	if err != nil {
		t.Fatalf("summarising the backup set: %v", err)
	}
	if set.Known != 3 || set.Done != 3 {
		t.Errorf("the set counts %d known and %d done, want every key counted", set.Known, set.Done)
	}
	if set.Bytes != 2000 {
		t.Errorf("the set measures %d bytes, want the shared photo once", set.Bytes)
	}
}

// The last whole walk is forgotten when what the library covers changes — its date or whether it
// is followed at all — and kept when only how it is walked does, or the weekly walk would be owed
// every time somebody ticked a box.
func TestChangingWhatTheLibraryCoversOwesAWholeWalk(t *testing.T) {
	since2020 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		instruction LibraryInstruction
		kept        bool
	}{
		"saving it unchanged":      {LibraryInstruction{Mode: SyncAll, Since: since2020}, true},
		"asking for new ones only": {LibraryInstruction{Mode: SyncAll, Since: since2020, NewOnly: true}, true},
		"moving the date back":     {LibraryInstruction{Mode: SyncAll, Since: since2020.AddDate(-5, 0, 0)}, false},
		"clearing the date":        {LibraryInstruction{Mode: SyncAll}, false},
		"no longer following it":   {LibraryInstruction{Mode: SyncNone, Since: since2020}, false},
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t)
			if err := store.SetLibrary(LibraryInstruction{Mode: SyncAll, Since: since2020}); err != nil {
				t.Fatalf("following the library: %v", err)
			}
			if err := store.MarkLibraryWalkedInFull(noon); err != nil {
				t.Fatalf("recording a whole walk: %v", err)
			}

			if err := store.SetLibrary(test.instruction); err != nil {
				t.Fatalf("saving the instruction: %v", err)
			}
			library, err := store.Library()
			if err != nil {
				t.Fatalf("reading the library: %v", err)
			}
			if kept := library.WalkedInFullAt.Equal(noon); kept != test.kept {
				t.Errorf("the last whole walk reads %s after the save; kept: %t, want %t", library.WalkedInFullAt, kept, test.kept)
			}
		})
	}
}

// An album's "to review" badge links to the queue, so it counts what the queue lists and the grid
// marks the same items. A flag on an arrival that has since been downloaded asks nothing, and
// counting it drew a badge pointing at a page with nothing on it.
func TestAnAlbumCountsOnlyTheReviewsTheQueueWillAsk(t *testing.T) {
	store := openTestStore(t)
	albumID := seedFollowedAlbum(t, store, SyncPicked, "arrived", "downloaded-since")
	if err := store.FlagForReview([]string{"arrived", "downloaded-since"}); err != nil {
		t.Fatalf("flagging the arrivals: %v", err)
	}
	landed := MediaItem{MediaKey: "downloaded-since", Filename: "IMG_0002.jpg", LocalPath: "2026/08/downloaded-since.jpg", SizeBytes: 12}
	if err := store.MarkDownloaded(landed, noon); err != nil {
		t.Fatalf("downloading one of them: %v", err)
	}

	stats, err := store.AlbumStats(albumID)
	if err != nil {
		t.Fatalf("summarising the album: %v", err)
	}
	waiting, err := store.CountNeedingReview()
	if err != nil {
		t.Fatalf("counting the queue: %v", err)
	}
	if stats.NeedsReview != 1 || waiting != 1 {
		t.Errorf("the album counts %d to review and the queue %d, want 1 each", stats.NeedsReview, waiting)
	}
	for key, want := range map[string]bool{"arrived": true, "downloaded-since": false} {
		item, err := store.Item(key)
		if err != nil {
			t.Fatalf("reading %s: %v", key, err)
		}
		if item.AwaitsReview() != want {
			t.Errorf("%s awaits review: %t, want %t", key, item.AwaitsReview(), want)
		}
	}
}

func TestTheBackupSetSaysHowMuchOfItIsVideo(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "photo-key", "video-key")
	if err := store.UpsertItem(MediaItem{MediaKey: "video-key", Filename: "clip.mp4", IsVideo: true}, noon); err != nil {
		t.Fatalf("marking the video: %v", err)
	}
	for key, size := range map[string]int64{"photo-key": 1000, "video-key": 5000} {
		item := MediaItem{MediaKey: key, Filename: key, LocalPath: "/pool/" + key, SizeBytes: size, SHA256: key}
		if err := store.MarkDownloaded(item, noon); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}

	set, err := store.BackupSet()
	if err != nil {
		t.Fatalf("summarising the backup set: %v", err)
	}
	if set.Bytes != 6000 || set.VideoBytes != 5000 {
		t.Errorf("the set measures %d bytes, %d of them video; want 6000 and 5000", set.Bytes, set.VideoBytes)
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
		if err := store.LinkItemToAlbum(albumID, key); err != nil {
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

// The pool name is the handle someone browsing the disk has, and it has to find its item by
// the whole name only: an underscore in it is not a wildcard, and a name that merely ends the
// same way is a different file.
func TestAnItemIsFoundByTheNameOfItsFile(t *testing.T) {
	s := openTestStore(t)
	at := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	for key, path := range map[string]string{
		"AF1QipKEY1": "/photos/pool/2026/2026-08/AF1QipKEY_PXL_1.MP.jpg",
		"AF1QipKEY2": "/photos/pool/2026/2026-08/xAF1QipKEY_PXL_1.MP.jpg",
	} {
		if err := s.UpsertItem(MediaItem{MediaKey: key, Filename: "PXL_1.MP.jpg", CapturedAt: at}, at); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		if err := s.MarkDownloaded(MediaItem{MediaKey: key, Filename: "PXL_1.MP.jpg", LocalPath: path, SizeBytes: 1, SHA256: "ab"}, at); err != nil {
			t.Fatalf("marking downloaded: %v", err)
		}
	}

	item, err := s.ItemStoredAs("AF1QipKEY_PXL_1.MP.jpg")
	if err != nil {
		t.Fatalf("finding by file name: %v", err)
	}
	if item.MediaKey != "AF1QipKEY1" {
		t.Errorf("found %s, want the item whose file has exactly that name", item.MediaKey)
	}
	if _, err := s.ItemStoredAs("AF1QipKEY%PXL_1.MP.jpg"); err == nil {
		t.Error("a % in the name matched as a wildcard")
	}
}

// A nightly walk re-lists everything the library already holds, and every one of those items used
// to be written back exactly as it was: 98,000 row updates and 98,000 link updates a night to say
// only that the walk had seen them again, which SQLite's write-ahead log turned into gigabytes of
// block I/O a day for a 52,000-item library.
//
// SQLite counts the rows each statement changes, so "a walk over unchanged contents writes
// nothing" is measurable rather than argued: a second identical walk has to leave the count
// exactly where the first one left it.
func TestAWalkOverUnchangedContentsChangesNothing(t *testing.T) {
	store := openTestStore(t)
	albumID := seedFollowedAlbum(t, store, SyncAll, "listed", "downloaded")

	// Most of a library is already on disk, which is the case that matters: a walk over an item
	// whose download state is the newest thing about it has to leave that alone too.
	done := MediaItem{MediaKey: "downloaded", Filename: "downloaded.jpg", CapturedAt: noon,
		LocalPath: "/pool/downloaded.jpg", SizeBytes: 4096, SHA256: "abc"}
	if err := store.MarkDownloaded(done, noon); err != nil {
		t.Fatalf("marking an item downloaded: %v", err)
	}

	walk := func(at time.Time) {
		t.Helper()
		for _, key := range []string{"listed", "downloaded"} {
			item := MediaItem{MediaKey: key, Filename: key + ".jpg", CapturedAt: noon}
			if err := store.UpsertItem(item, at); err != nil {
				t.Fatalf("re-listing %s: %v", key, err)
			}
			if err := store.LinkItemToAlbum(albumID, key); err != nil {
				t.Fatalf("re-linking %s: %v", key, err)
			}
		}
	}

	walk(noon)
	before := changedRows(t, store)
	walk(noon.Add(24 * time.Hour))

	if after := changedRows(t, store); after != before {
		t.Errorf("a second walk over the same contents changed %d rows, want none", after-before)
	}

	item, err := store.Item("downloaded")
	if err != nil {
		t.Fatalf("reading the downloaded item: %v", err)
	}
	if item.State != StateDone || item.LocalPath != "/pool/downloaded.jpg" {
		t.Errorf("a re-listed item is %q at %q, want the download the walk found",
			item.State, item.LocalPath)
	}
}

// changedRows is SQLite's own count of what this connection has inserted, updated or deleted.
func changedRows(t *testing.T, store *Store) int {
	t.Helper()

	var changes int
	if err := store.db.QueryRow(`SELECT total_changes()`).Scan(&changes); err != nil {
		t.Fatalf("reading SQLite's change count: %v", err)
	}
	return changes
}

// The listing is the only source of the capture date, the thumbnail URL and the video flag, so
// skipping the write for an item that is unchanged must not become skipping it for one that is
// not. Each case changes one field alone: the write is guarded by one condition per field, and a
// change riding along with another would hide the loss of its own.
func TestARelistingStillRefreshesWhatTheListingOwns(t *testing.T) {
	for _, change := range []struct {
		field string
		apply func(*MediaItem)
	}{
		{"capture date", func(item *MediaItem) { item.CapturedAt = noon.Add(time.Hour) }},
		{"thumbnail", func(item *MediaItem) { item.ThumbnailURL = "https://lh3.example/new" }},
		{"video flag", func(item *MediaItem) { item.IsVideo = true }},
	} {
		t.Run(change.field, func(t *testing.T) {
			store := openTestStore(t)
			seedFollowedAlbum(t, store, SyncAll, "item-1")

			relisted := relistedUnchanged
			change.apply(&relisted)
			if err := store.UpsertItem(relisted, noon.Add(24*time.Hour)); err != nil {
				t.Fatalf("re-listing the item: %v", err)
			}

			item, err := store.Item("item-1")
			if err != nil {
				t.Fatalf("reading the item: %v", err)
			}
			if !item.CapturedAt.Equal(relisted.CapturedAt) || item.ThumbnailURL != relisted.ThumbnailURL ||
				item.IsVideo != relisted.IsVideo {
				t.Errorf("a re-listing that changed the %s left the item at captured %s, thumbnail %q, video %t; want %s, %q, %t",
					change.field, item.CapturedAt, item.ThumbnailURL, item.IsVideo,
					relisted.CapturedAt, relisted.ThumbnailURL, relisted.IsVideo)
			}
		})
	}
}

// Migrations are a one-way trip taken on data somebody already has, and they are taken at startup:
// one that cannot run leaves the backup not merely stale but not running at all. So this is the
// upgrade path itself — a database at the schema as it stood at the last release, holding rows,
// opened by the code as it is now.
//
// The schema is the one 0.4.2 left, built by replaying its sixteen migrations rather than by adding
// the dropped columns back onto today's: a migration runs against exactly what its predecessors
// left, constraints and all, and the rows are written in that release's terms for the same reason.
func TestTheMigrationsRunOnADatabaseThatAlreadyHasRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	lastRelease := storeAtVersion(t, path, 16)

	seen := formatTime(noon)
	for _, statement := range []string{
		`INSERT INTO albums (id, title, sync_mode, first_seen_at, last_seen_at) VALUES ('album-1', 'Holiday', 'all', ?1, ?1)`,
		`INSERT INTO media_items (media_key, filename, captured_at, mime_type, state, local_path, first_seen_at, last_seen_at)
		VALUES ('item-1', 'item-1.jpg', ?1, 'image/jpeg', 'done', '2026/08/item-1.jpg', ?1, ?1),
		       ('item-2', 'item-2.mp4', ?1, 'video/mp4', 'discovered', NULL, ?1, ?1)`,
		`INSERT INTO album_items (album_id, media_key, last_seen_at) VALUES ('album-1', 'item-1', ?1), ('album-1', 'item-2', ?1)`,
		`UPDATE albums SET sync_mode = 'all', last_synced_at = ?1 WHERE id = 'library'`,
	} {
		if _, err := lastRelease.db.Exec(statement, seen); err != nil {
			t.Fatalf("writing the rows 0.4.2 would have held: %v", err)
		}
	}
	if err := lastRelease.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("opening a database the migrations have to upgrade: %v", err)
	}
	defer upgraded.Close()

	members, err := upgraded.AlbumMembers("album-1")
	if err != nil {
		t.Fatalf("reading the membership after the migrations: %v", err)
	}
	if len(members) != 2 {
		t.Errorf("the album holds %d members after the migrations, want the 2 it held before", len(members))
	}
	item, err := upgraded.Item("item-1")
	if err != nil {
		t.Fatalf("an item did not survive the migrations: %v", err)
	}
	if item.State != StateDone || item.LocalPath != "2026/08/item-1.jpg" || !item.CapturedAt.Equal(noon) {
		t.Errorf("a downloaded item came through the migrations as %s at %q captured %s, want done at its path, captured at noon",
			item.State, item.LocalPath, item.CapturedAt)
	}

	// Every walk 0.4.2 made was a whole one, so the last of them is the last whole walk, and the
	// weekly one is not owed the first night after upgrading.
	library, err := upgraded.Library()
	if err != nil {
		t.Fatalf("reading the library after the migrations: %v", err)
	}
	if !library.WalkedInFullAt.Equal(noon) || library.NewOnly || !library.WeeklyFullWalk {
		t.Errorf("the library came through as walked whole %s, new only %t, weekly %t; want noon, false, true",
			library.WalkedInFullAt, library.NewOnly, library.WeeklyFullWalk)
	}

	for table, dropped := range map[string][]string{
		"album_items": {"last_seen_at"},
		"media_items": {"last_seen_at", "mime_type"},
	} {
		columns, err := tableColumns(t, upgraded, table)
		if err != nil {
			t.Fatalf("reading the columns of %s: %v", table, err)
		}
		for _, column := range dropped {
			if slices.Contains(columns, column) {
				t.Errorf("%s still carries %s after the migrations", table, column)
			}
		}
	}
}

// storeAtVersion builds the database a release left behind by replaying the migrations up to its
// last one, as that release's own Open did.
func storeAtVersion(t *testing.T, path string, version int) *Store {
	t.Helper()

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}

	migrations, err := pendingMigrations(0)
	if err != nil {
		t.Fatalf("reading the migrations: %v", err)
	}
	for _, migration := range migrations {
		if migration.version > version {
			break
		}
		if err := store.apply(migration); err != nil {
			t.Fatalf("applying migration %s: %v", migration.name, err)
		}
	}
	return store
}

func tableColumns(t *testing.T, store *Store, table string) ([]string, error) {
	t.Helper()

	rows, err := store.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}
