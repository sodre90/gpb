package store

import (
	"testing"
)

// A backed-up file is read for its place once; the reading is remembered whether or not it
// found one, and the grid can then be narrowed to a box on the map.
func TestLocationsAreReadOnceAndNarrowTheGrid(t *testing.T) {
	store := openTestStore(t)
	seedFollowedAlbum(t, store, SyncAll, "island", "city", "nowhere", "not-yet-fetched")
	for _, key := range []string{"island", "city", "nowhere"} {
		if err := store.MarkDownloaded(MediaItem{MediaKey: key, Filename: key + ".jpg", LocalPath: "/pool/" + key}, noon); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}

	waiting, err := store.Unlocated(10)
	if err != nil {
		t.Fatalf("listing unlocated files: %v", err)
	}
	if len(waiting) != 3 {
		t.Fatalf("%d files wait to be read, want the 3 on disk", len(waiting))
	}
	progress, _ := store.LocationProgress()
	if progress.Finished() || progress.OnDisk != 3 || progress.Read != 0 {
		t.Errorf("before any reading the progress is %+v", progress)
	}

	err = store.MarkLocated([]Location{
		{MediaKey: "island", Latitude: 28.4, Longitude: -14.0, Known: true},
		{MediaKey: "city", Latitude: 47.5, Longitude: 19.0, Known: true},
		{MediaKey: "nowhere"},
	}, noon)
	if err != nil {
		t.Fatalf("recording locations: %v", err)
	}

	waiting, _ = store.Unlocated(10)
	if len(waiting) != 0 {
		t.Errorf("%d files still wait after every one on disk was read", len(waiting))
	}
	progress, _ = store.LocationProgress()
	if !progress.Finished() || progress.Read != 3 || progress.Located != 2 {
		t.Errorf("after reading the progress is %+v", progress)
	}

	canaries := Where{Within: &Area{South: 27.9, North: 28.8, West: -14.6, East: -13.8}}
	count, _ := store.EveryItemCount(canaries)
	items, _ := store.EveryItemPage(canaries, 0, 10)
	months, _ := store.EveryItemMonths(canaries)
	if count != 1 || len(items) != 1 || items[0].MediaKey != "island" || len(months) != 1 || months[0].Count != 1 {
		t.Errorf("the box holds %d items, page %v, months %v; want the island alone", count, items, months)
	}
	everything, _ := store.EveryItemCount(Where{})
	if everything != 4 {
		t.Errorf("no box narrows to %d, want all 4", everything)
	}
}

// A place asked about once is remembered, found or not, under the name as typed give or take
// case and spacing.
func TestPlacesAreRememberedAsTyped(t *testing.T) {
	store := openTestStore(t)

	if _, found, err := store.CachedPlace("Fuerteventura"); err != nil || found {
		t.Fatalf("an unasked name was remembered: found=%v err=%v", found, err)
	}

	island := Place{Query: "Fuerteventura", Name: "Fuerteventura, Canarias, España",
		Area: Area{South: 27.9, North: 28.8, West: -14.6, East: -13.8}, Found: true, LookedUpAt: noon}
	if err := store.RememberPlace(island); err != nil {
		t.Fatalf("remembering a place: %v", err)
	}
	if err := store.RememberPlace(Place{Query: "Atlantis", LookedUpAt: noon}); err != nil {
		t.Fatalf("remembering an unknown place: %v", err)
	}

	remembered, found, err := store.CachedPlace("  fuerteventura ")
	if err != nil || !found {
		t.Fatalf("the island was not remembered: found=%v err=%v", found, err)
	}
	if !remembered.Found || remembered.Name != island.Name || remembered.Area != island.Area || !remembered.LookedUpAt.Equal(noon) {
		t.Errorf("the island came back as %+v", remembered)
	}
	unknown, found, _ := store.CachedPlace("atlantis")
	if !found || unknown.Found {
		t.Errorf("the unknown name came back as found=%v %+v", found, unknown)
	}
}
