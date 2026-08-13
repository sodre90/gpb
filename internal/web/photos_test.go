package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
)

func seedLooseItem(t *testing.T, server *Server, key string, capturedAt time.Time) {
	t.Helper()

	item := store.MediaItem{
		MediaKey:     key,
		Filename:     key + ".jpg",
		CapturedAt:   capturedAt,
		ThumbnailURL: "https://photos.fife.usercontent.google.com/pw/" + key,
	}
	if err := server.store.UpsertItem(item, time.Now()); err != nil {
		t.Fatalf("seeding %s: %v", key, err)
	}
}

// Most of a real library is in no album at all, so a page that only reached items through
// albums would be missing the majority of what has been backed up.
func TestThePhotoPageShowsItemsNoAlbumHolds(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 2)
	seedLooseItem(t, server, "AF1QipLoose", time.Now().Add(time.Hour))

	recorder := get(handler, "/photos", login(t, handler))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /photos returned %d, want 200", recorder.Code)
	}

	body := recorder.Body.String()
	for _, want := range []string{"/thumb/AF1Qip0000", "/thumb/AF1Qip0001", "/thumb/AF1QipLoose"} {
		if !strings.Contains(body, want) {
			t.Errorf("the photo page is missing %q", want)
		}
	}
}

// Newest first, unlike an album: someone looking through everything is far more often after
// something recent than after the oldest thing they own.
func TestThePhotoPageLeadsWithTheNewest(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 3)

	body := get(handler, "/photos", login(t, handler)).Body.String()
	newest := strings.Index(body, "/thumb/AF1Qip0002")
	oldest := strings.Index(body, "/thumb/AF1Qip0000")
	if newest < 0 || oldest < 0 || newest > oldest {
		t.Errorf("the newest item is at %d and the oldest at %d, want the newest first", newest, oldest)
	}
}

// A tick here could not say which album it meant — an item can sit in several — so the page
// shows and does not decide, and must not draw a checkbox that goes nowhere.
func TestThePhotoPageDoesNotOfferPicking(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 2)

	body := get(handler, "/photos", login(t, handler)).Body.String()
	if strings.Contains(body, `type="checkbox"`) {
		t.Error("the photo page rendered checkboxes")
	}
	if !strings.Contains(body, "data-open") {
		t.Error("the photo page rendered no way to open a photo")
	}
}

func TestThePhotoPagePagesTheWholeAccount(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", pageSize+10)
	cookie := login(t, handler)

	first := get(handler, "/photos", cookie).Body.String()
	if cells := strings.Count(first, `class="grid-cell`); cells != pageSize {
		t.Errorf("page one rendered %d cells, want %d", cells, pageSize)
	}
	if !strings.Contains(first, "/photos?page=2") {
		t.Error("page one offered no way to the next page")
	}

	second := get(handler, "/photos?page=2", cookie).Body.String()
	if cells := strings.Count(second, `class="grid-cell`); cells != 10 {
		t.Errorf("page two rendered %d cells, want 10", cells)
	}
}

func TestAnEmptyAccountSaysSoRatherThanPagingNothing(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	body := get(handler, "/photos", login(t, handler)).Body.String()
	if !strings.Contains(body, "Nothing has been listed yet") {
		t.Error("an empty photo page did not explain itself")
	}
	if strings.Contains(body, "Page 1 of 0") {
		t.Error("an empty photo page paged nothing")
	}
}

func TestOutOfRangePhotoPagesLandOnTheLastOne(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", pageSize+10)

	body := get(handler, fmt.Sprintf("/photos?page=%d", 99), login(t, handler)).Body.String()
	if !strings.Contains(body, "Page 2 of 2") {
		t.Error("a page past the end did not land on the last page")
	}
}
