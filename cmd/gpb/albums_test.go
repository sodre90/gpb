package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
)

func TestResolveAlbumAcceptsAnyUniquePrefix(t *testing.T) {
	db := storeWithAlbums(t, "AF1QipHoliday", "AF1QipFamily", "BZ2Weekend")

	for _, prefix := range []string{"AF1QipHoliday", "AF1QipH", "AF1QipHoliday"[:8]} {
		album, err := resolveAlbum(db, prefix)
		if err != nil {
			t.Fatalf("resolving %q: %v", prefix, err)
		}
		if album.ID != "AF1QipHoliday" {
			t.Fatalf("prefix %q resolved to %q", prefix, album.ID)
		}
	}
}

// The listing truncates ids with an ellipsis, so the obvious thing a user does is copy the
// cell as printed. Accepting that spelling costs one TrimSuffix and saves a confusing refusal.
func TestResolveAlbumAcceptsTheEllipsisFromTheListing(t *testing.T) {
	db := storeWithAlbums(t, "AF1QipSomethingLong")

	album, err := resolveAlbum(db, shortID("AF1QipSomethingLong"))
	if err != nil {
		t.Fatalf("resolving a printed id: %v", err)
	}
	if album.ID != "AF1QipSomethingLong" {
		t.Fatalf("resolved to %q", album.ID)
	}
}

// An ambiguous prefix must never be guessed: following the wrong album would only surface as
// unexpected download traffic, long after the mistake.
func TestResolveAlbumRefusesAnAmbiguousPrefix(t *testing.T) {
	db := storeWithAlbums(t, "AF1QipHoliday", "AF1QipFamily")

	if _, err := resolveAlbum(db, "AF1Qip"); err == nil {
		t.Fatal("an ambiguous prefix resolved instead of failing")
	} else if !strings.Contains(err.Error(), "matches 2 albums") {
		t.Fatalf("the error does not say what went wrong: %v", err)
	}
}

func TestResolveAlbumRejectsAnUnknownPrefix(t *testing.T) {
	db := storeWithAlbums(t, "AF1QipHoliday")

	if _, err := resolveAlbum(db, "nothing"); err == nil {
		t.Fatal("an unknown prefix resolved instead of failing")
	}
}

func TestPrintAlbumsShowsModeAndCount(t *testing.T) {
	printed := capture(t, func() {
		printAlbums([]store.Album{
			{ID: "AF1QipHoliday", Title: "Holiday", ItemCount: 1150, SyncMode: store.SyncAll},
			{ID: "AF1QipFamily", ItemCount: 312, SyncMode: store.SyncNone},
		})
	})

	for _, want := range []string{"Holiday", "all", "1150", "(untitled)", "2 albums, 1 followed"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the listing does not mention %q:\n%s", want, printed)
		}
	}
}

func TestHumanBytesScales(t *testing.T) {
	for input, want := range map[int64]string{
		0:             "0 B",
		999:           "999 B",
		1500:          "1.5 kB",
		2_100_000_000: "2.1 GB",
	} {
		if got := humanBytes(input); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", input, got, want)
		}
	}
}

func storeWithAlbums(t *testing.T, ids ...string) *store.Store {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, id := range ids {
		if err := db.UpsertAlbum(store.Album{ID: id, Title: id}, time.Now()); err != nil {
			t.Fatalf("seeding album %s: %v", id, err)
		}
	}
	return db
}

func capture(t *testing.T, print func()) string {
	t.Helper()

	buffer := &bytes.Buffer{}
	previous := stdout
	stdout = buffer
	defer func() { stdout = previous }()

	print()
	return buffer.String()
}
