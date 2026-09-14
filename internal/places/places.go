// Package places turns a name into a box on the map, by asking a Nominatim server —
// OpenStreetMap's own by default. It is the one thing in this program that talks to anyone
// but Google, and what it sends is the name that was typed and nothing else: no photo, no
// coordinate, no account. The answer is remembered by the store, so a name is asked once.
package places

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"gpb/internal/store"
	"gpb/internal/version"
)

// requestGap is what the public OpenStreetMap server's usage policy asks between requests,
// along with an agent that names the application. Both are kept.
const requestGap = time.Second

// Lookup asks a Nominatim server for places by name.
type Lookup struct {
	url    string
	client *http.Client

	mu       sync.Mutex
	lastSent time.Time
}

// New returns a lookup against the server at url, or nil for an empty url: a lookup nobody
// configured is a lookup nobody wanted, and the page then offers no search.
func New(url string) *Lookup {
	if url == "" {
		return nil
	}
	return &Lookup{url: url, client: &http.Client{Timeout: 20 * time.Second}}
}

// answer is the part of a Nominatim result that matters here. The box arrives as four
// strings, south, north, west, east, which is the order the store keeps them in too.
type answer struct {
	DisplayName string   `json:"display_name"`
	BoundingBox []string `json:"boundingbox"`
}

// Find returns the first place the server offers for the name, or a Place with Found false
// when it offers none. Anything that stops the question being answered at all is an error,
// so that a server being down is not remembered as the place not existing.
func (l *Lookup) Find(ctx context.Context, name string) (store.Place, error) {
	l.pace(ctx)

	query := url.Values{"q": {name}, "format": {"jsonv2"}, "limit": {"1"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url+"?"+query.Encode(), nil)
	if err != nil {
		return store.Place{}, err
	}
	request.Header.Set("User-Agent", "gpb/"+version.Current+" (https://github.com/sodre90/gpb)")
	request.Header.Set("Accept", "application/json")

	response, err := l.client.Do(request)
	if err != nil {
		return store.Place{}, fmt.Errorf("asking about a place: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return store.Place{}, fmt.Errorf("asking about a place: the server answered %s", response.Status)
	}

	var answers []answer
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&answers); err != nil {
		return store.Place{}, fmt.Errorf("reading the answer about a place: %w", err)
	}
	place := store.Place{Query: name, LookedUpAt: time.Now()}
	if len(answers) == 0 {
		return place, nil
	}
	area, err := areaOf(answers[0].BoundingBox)
	if err != nil {
		return store.Place{}, fmt.Errorf("reading the answer about a place: %w", err)
	}
	place.Name, place.Area, place.Found = answers[0].DisplayName, area, true
	return place, nil
}

func areaOf(box []string) (store.Area, error) {
	if len(box) != 4 {
		return store.Area{}, errors.New("the bounding box does not have four edges")
	}
	var edges [4]float64
	for i, edge := range box {
		value, err := strconv.ParseFloat(edge, 64)
		if err != nil {
			return store.Area{}, fmt.Errorf("the bounding box edge %q is not a number", edge)
		}
		edges[i] = value
	}
	return store.Area{South: edges[0], North: edges[1], West: edges[2], East: edges[3]}, nil
}

// pace holds a request back until a second has passed since the last one left, which is what
// the public server's policy asks. Two searches typed at once are answered one after the other.
func (l *Lookup) pace(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if wait := requestGap - time.Since(l.lastSent); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	l.lastSent = time.Now()
}
