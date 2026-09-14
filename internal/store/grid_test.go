package store

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

// seedDatedAlbum fills one album with items captured at the given instants, keyed in the
// order given, plus any undated ones named.
func seedDatedAlbum(t *testing.T, store *Store, captures map[string]time.Time) string {
	t.Helper()

	const albumID = "album-dated"
	if err := store.UpsertAlbum(Album{ID: albumID, Title: "Dated", ItemCount: len(captures)}, noon); err != nil {
		t.Fatalf("seeding the album: %v", err)
	}
	for key, capturedAt := range captures {
		if err := store.UpsertItem(MediaItem{MediaKey: key, Filename: key + ".jpg", CapturedAt: capturedAt}, noon); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
		if err := store.LinkItemToAlbum(albumID, key, noon); err != nil {
			t.Fatalf("linking %s: %v", key, err)
		}
	}
	return albumID
}

// A month header over a cell whose caption names the previous month would read as a bug, so
// the fold has to use the caption's clock — local time — rather than the stored UTC.
func TestMonthsFoldOnTheCaptionsClock(t *testing.T) {
	// The fold happens inside SQLite, which reads the zone from the environment the way libc
	// does, so the environment is what the test sets; TZ is how the daemon is told its zone too.
	t.Setenv("TZ", "Europe/Budapest")

	store := openTestStore(t)
	// 23:30 UTC on 31 July is 01:30 on 1 August in Budapest.
	seedDatedAlbum(t, store, map[string]time.Time{
		"late-july":  time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC),
		"mid-july":   time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
		"mid-august": time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
	})

	months, err := store.EveryItemMonths()
	if err != nil {
		t.Fatalf("folding by month: %v", err)
	}
	want := []MonthCount{{Month: "2026-08", Count: 2}, {Month: "2026-07", Count: 1}}
	if !reflect.DeepEqual(months, want) {
		t.Errorf("the library folded to %+v, want %+v", months, want)
	}
}

// The months are what a page lays the grid out from, so their order and their undated bucket
// have to agree with the item order exactly: the library runs newest first with undated items
// last, an album oldest first with undated items first.
func TestMonthsFollowEachGridsOrderIncludingUndatedItems(t *testing.T) {
	store := openTestStore(t)
	albumID := seedDatedAlbum(t, store, map[string]time.Time{
		"a-2024":  time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC),
		"b-2024":  time.Date(2024, 3, 2, 12, 0, 0, 0, time.UTC),
		"c-2026":  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		"undated": {},
	})

	library, err := store.EveryItemMonths()
	if err != nil {
		t.Fatalf("folding the library: %v", err)
	}
	wantLibrary := []MonthCount{{"2026-01", 1}, {"2024-03", 2}, {"", 1}}
	if !reflect.DeepEqual(library, wantLibrary) {
		t.Errorf("the library folded to %+v, want %+v", library, wantLibrary)
	}
	page, err := store.EveryItemPage(0, 10)
	if err != nil {
		t.Fatalf("paging the library: %v", err)
	}
	if got := keysOf(page); !slices.Equal(got, []string{"c-2026", "b-2024", "a-2024", "undated"}) {
		t.Errorf("the library page runs %v, which the months do not describe", got)
	}

	album, err := store.AlbumMonths(albumID)
	if err != nil {
		t.Fatalf("folding the album: %v", err)
	}
	wantAlbum := []MonthCount{{"", 1}, {"2024-03", 2}, {"2026-01", 1}}
	if !reflect.DeepEqual(album, wantAlbum) {
		t.Errorf("the album folded to %+v, want %+v", album, wantAlbum)
	}
	page, err = store.AlbumPage(albumID, 0, 10)
	if err != nil {
		t.Fatalf("paging the album: %v", err)
	}
	if got := keysOf(page); !slices.Equal(got, []string{"undated", "a-2024", "b-2024", "c-2026"}) {
		t.Errorf("the album page runs %v, which the months do not describe", got)
	}
}

// A shift-click names its two ends and the store fills in the rest, in either direction,
// across undated items, and never beyond the album.
func TestSelectRangeFillsInTheCellsBetweenTwoEnds(t *testing.T) {
	store := openTestStore(t)
	albumID := seedDatedAlbum(t, store, map[string]time.Time{
		"undated": {},
		"2024-a":  time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC),
		"2024-b":  time.Date(2024, 3, 2, 12, 0, 0, 0, time.UTC),
		"2025":    time.Date(2025, 3, 2, 12, 0, 0, 0, time.UTC),
		"2026":    time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC),
	})
	elsewhere := MediaItem{MediaKey: "other-album", Filename: "x.jpg", CapturedAt: time.Date(2024, 3, 1, 18, 0, 0, 0, time.UTC)}
	if err := store.UpsertItem(elsewhere, noon); err != nil {
		t.Fatalf("seeding an item outside the album: %v", err)
	}

	if err := store.SelectRange(albumID, "2025", "undated", true); err != nil {
		t.Fatalf("selecting backwards across the undated item: %v", err)
	}
	picked, err := store.SelectionIn(albumID)
	if err != nil {
		t.Fatalf("reading the selection: %v", err)
	}
	want := map[string]bool{"undated": true, "2024-a": true, "2024-b": true, "2025": true}
	if !reflect.DeepEqual(picked, want) {
		t.Errorf("the range picked %v, want %v", picked, want)
	}

	if err := store.SelectRange(albumID, "2024-a", "2024-b", false); err != nil {
		t.Fatalf("clearing a range: %v", err)
	}
	picked, err = store.SelectionIn(albumID)
	if err != nil {
		t.Fatalf("reading the selection: %v", err)
	}
	want = map[string]bool{"undated": true, "2025": true}
	if !reflect.DeepEqual(picked, want) {
		t.Errorf("after clearing the middle the selection is %v, want %v", picked, want)
	}
}
