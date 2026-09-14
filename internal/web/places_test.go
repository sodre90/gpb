package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
)

// atlas answers the way a geocoder does, from a map of names it knows, and counts what it was
// asked so a test can tell a remembered answer from a fresh question.
type atlas struct {
	known map[string]store.Place
	asked []string
	down  bool
}

func (a *atlas) Find(ctx context.Context, name string) (store.Place, error) {
	a.asked = append(a.asked, name)
	if a.down {
		return store.Place{}, errors.New("the geocoder is not answering")
	}
	place, ok := a.known[strings.ToLower(name)]
	if !ok {
		return store.Place{Query: name, LookedUpAt: time.Now()}, nil
	}
	place.Query, place.LookedUpAt = name, time.Now()
	return place, nil
}

var fuerteventura = store.Place{Name: "Fuerteventura, Las Palmas, Canarias, España", Found: true,
	Area: store.Area{South: 27.9, North: 28.8, West: -14.6, East: -13.8}}

func seedPlacedPhotos(t *testing.T, server *Server) {
	t.Helper()
	seedLooseItem(t, server, "AF1QipIsland", time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC))
	seedLooseItem(t, server, "AF1QipCity", time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC))
	seedLooseItem(t, server, "AF1QipUnread", time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))
	for _, key := range []string{"AF1QipIsland", "AF1QipCity", "AF1QipUnread"} {
		if err := server.store.MarkDownloaded(store.MediaItem{MediaKey: key, Filename: key + ".jpg", LocalPath: "/pool/" + key}, time.Now()); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}
	if err := server.store.MarkLocated([]store.Location{
		{MediaKey: "AF1QipIsland", Latitude: 28.4, Longitude: -14.0, Known: true},
		{MediaKey: "AF1QipCity", Latitude: 47.5, Longitude: 19.0, Known: true},
	}, time.Now()); err != nil {
		t.Fatalf("recording locations: %v", err)
	}
}

// A name typed into the search narrows the grid, its timeline and its windows to the box the
// geocoder answered with, and the page says which place that was.
func TestThePhotosPageIsNarrowedToAPlace(t *testing.T) {
	server, _ := testServer(t)
	geocoder := &atlas{known: map[string]store.Place{"fuerteventura": fuerteventura}}
	server.places = geocoder
	handler := server.Handler()
	cookie := login(t, handler)
	seedPlacedPhotos(t, server)

	body := get(handler, "/photos?place=Fuerteventura", cookie).Body.String()
	if !strings.Contains(body, "1 photo in <strong>Fuerteventura, Las Palmas, Canarias, España</strong>") {
		t.Errorf("the page does not say what it found where: %s", excerpt(body, "photo in"))
	}
	if !strings.Contains(body, `data-key="AF1QipIsland"`) || strings.Contains(body, `data-key="AF1QipCity"`) {
		t.Error("the grid is not narrowed to the island")
	}
	if !strings.Contains(body, `data-cells="/photos/cells?place=Fuerteventura"`) || !strings.Contains(body, `data-total="1"`) {
		t.Errorf("the timeline is not narrowed with the grid: %s", excerpt(body, "data-cells"))
	}
	if !strings.Contains(body, "2 of 3 so far, 2 with a place") {
		t.Errorf("the page does not say how far the reading has got: %s", excerpt(body, "so far"))
	}
	if !strings.Contains(body, `value="Fuerteventura"`) {
		t.Error("the search box does not keep what was typed")
	}

	window := get(handler, "/photos/cells?place=Fuerteventura&offset=0&limit=10", cookie).Body.String()
	if !strings.Contains(window, `data-key="AF1QipIsland"`) || strings.Contains(window, `data-key="AF1QipCity"`) {
		t.Error("a window of cells is not narrowed to the island")
	}

	get(handler, "/photos?place=fuerteventura", cookie)
	if len(geocoder.asked) != 1 {
		t.Errorf("the geocoder was asked %d times for one name, want once", len(geocoder.asked))
	}
}

func TestANameTheGeocoderDoesNotKnowSaysSo(t *testing.T) {
	server, _ := testServer(t)
	geocoder := &atlas{known: map[string]store.Place{}}
	server.places = geocoder
	handler := server.Handler()
	cookie := login(t, handler)
	seedPlacedPhotos(t, server)

	body := get(handler, "/photos?place=Atlantis", cookie).Body.String()
	if !strings.Contains(body, "knows no place called “Atlantis”") {
		t.Errorf("the page does not say the name is unknown: %s", excerpt(body, "Atlantis"))
	}
	if strings.Contains(body, `data-key="`) {
		t.Error("an unknown place shows photos")
	}
	get(handler, "/photos?place=Atlantis", cookie)
	if len(geocoder.asked) != 1 {
		t.Errorf("an unknown name was asked about %d times, want once", len(geocoder.asked))
	}
}

// A geocoder that is down is not a name that does not exist: the page says the lookup could
// not be made, and remembers nothing, so the next try asks again.
func TestAGeocoderThatIsDownIsNotAnAnswer(t *testing.T) {
	server, _ := testServer(t)
	geocoder := &atlas{down: true}
	server.places = geocoder
	handler := server.Handler()
	cookie := login(t, handler)

	body := get(handler, "/photos?place=Fuerteventura", cookie).Body.String()
	if !strings.Contains(body, "could not be looked up just now") {
		t.Errorf("the page does not say the lookup failed: %s", excerpt(body, "looked up"))
	}
	get(handler, "/photos?place=Fuerteventura", cookie)
	if len(geocoder.asked) != 2 {
		t.Errorf("a failed lookup was asked about %d times over two loads, want both", len(geocoder.asked))
	}
}

func TestWithoutALookupThePhotosPageOffersNoSearch(t *testing.T) {
	server, _ := testServer(t)
	server.places = nil
	handler := server.Handler()
	cookie := login(t, handler)
	seedPlacedPhotos(t, server)

	body := get(handler, "/photos?place=Fuerteventura", cookie).Body.String()
	if strings.Contains(body, `name="place"`) {
		t.Error("a search is offered with no lookup configured")
	}
	if !strings.Contains(body, `data-key="AF1QipCity"`) {
		t.Error("with no lookup the grid is not the whole library")
	}
	if recorder := get(handler, "/photos?place=Fuerteventura", cookie); recorder.Code != http.StatusOK {
		t.Errorf("a search with no lookup answered %d", recorder.Code)
	}
}

func excerpt(body, around string) string {
	at := strings.Index(body, around)
	if at < 0 {
		return "(not present)"
	}
	return body[max(0, at-120):min(len(body), at+160)]
}
