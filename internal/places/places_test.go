package places

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The server is a stand-in for Nominatim answering the way it does: a list, each entry with
// a display name and a box of four strings.
func serverAnswering(t *testing.T, body string, seen *[]*http.Request) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}

func TestAKnownNameComesBackAsANamedBox(t *testing.T) {
	var seen []*http.Request
	server := serverAnswering(t, `[{"display_name":"Fuerteventura, Las Palmas, Canarias, España",
		"boundingbox":["27.9","28.8","-14.6","-13.8"],"lat":"28.3","lon":"-14.0"}]`, &seen)
	defer server.Close()

	place, err := New(server.URL).Find(t.Context(), "Fuerteventura")
	if err != nil {
		t.Fatalf("looking up a place: %v", err)
	}
	if !place.Found || place.Name != "Fuerteventura, Las Palmas, Canarias, España" {
		t.Errorf("the place came back as %+v", place)
	}
	if place.Area.South != 27.9 || place.Area.North != 28.8 || place.Area.West != -14.6 || place.Area.East != -13.8 {
		t.Errorf("the box came back as %+v", place.Area)
	}

	request := seen[0]
	if request.URL.Query().Get("q") != "Fuerteventura" || request.URL.Query().Get("format") != "jsonv2" {
		t.Errorf("the server was asked %s", request.URL.RawQuery)
	}
	// The public server's policy: an agent that says who is asking, and nothing else about them.
	if !strings.HasPrefix(request.Header.Get("User-Agent"), "gpb/") {
		t.Errorf("the request went out as %q", request.Header.Get("User-Agent"))
	}
	if len(request.URL.Query()) != 3 {
		t.Errorf("the request carried more than the name: %s", request.URL.RawQuery)
	}
}

func TestAnUnknownNameIsAnAnswerAndAServerErrorIsNot(t *testing.T) {
	var seen []*http.Request
	server := serverAnswering(t, `[]`, &seen)
	defer server.Close()

	place, err := New(server.URL).Find(t.Context(), "Nowhere In Particular")
	if err != nil {
		t.Fatalf("looking up a place: %v", err)
	}
	if place.Found {
		t.Errorf("an unknown name came back found: %+v", place)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "over capacity", http.StatusServiceUnavailable)
	}))
	defer broken.Close()
	if _, err := New(broken.URL).Find(t.Context(), "Fuerteventura"); err == nil {
		t.Error("a server that could not answer was read as an answer")
	}
}

func TestRequestsAreASecondApart(t *testing.T) {
	var seen []*http.Request
	server := serverAnswering(t, `[]`, &seen)
	defer server.Close()

	lookup := New(server.URL)
	start := time.Now()
	for range 2 {
		if _, err := lookup.Find(t.Context(), "anywhere"); err != nil {
			t.Fatalf("looking up a place: %v", err)
		}
	}
	if took := time.Since(start); took < requestGap {
		t.Errorf("two requests went out %v apart, want at least %v", took, requestGap)
	}
}

func TestNoURLIsNoLookup(t *testing.T) {
	if New("") != nil {
		t.Error("an empty url made a lookup")
	}
}
