package web

import (
	"context"
	"log"
	"net/http"
	"strings"

	"gpb/internal/places"
	"gpb/internal/store"
)

// PlaceFinder is what turns a typed name into a box on the map. *places.Lookup is the one in
// use; it is an interface so a test can answer without a geocoder.
type PlaceFinder interface {
	Find(ctx context.Context, name string) (store.Place, error)
}

// placeFinderFor is nil for an empty URL. A typed nil *places.Lookup in the interface would
// not be nil, which is the whole reason this function exists.
func placeFinderFor(url string) PlaceFinder {
	if lookup := places.New(url); lookup != nil {
		return lookup
	}
	return nil
}

// placeSearch is the search as the Photos page shows it: whether one is offered at all, what
// was typed, and what came of it.
type placeSearch struct {
	Offered bool
	Query   string
	// Name is what the geocoder calls the place it matched, shown so that a wrong first match
	// is visible rather than a mystery.
	Name  string
	Known bool
	// Unavailable is a lookup that could not be made — the geocoder down, the box offline —
	// which is not the same as a name it did not know, and is not remembered as one.
	Unavailable bool
}

// Found reports whether the grid is narrowed to a place.
func (p placeSearch) Found() bool { return p.Query != "" && p.Known }

func placeQuery(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("place"))
}

// searchFor resolves what was typed: the store's memory first, the geocoder once for a name
// it has not seen, and the answer remembered either way.
func (s *Server) searchFor(ctx context.Context, query string) (placeSearch, store.Where) {
	if s.places == nil {
		return placeSearch{}, store.Where{} // a place in the URL of a page with no search is ignored
	}
	search := placeSearch{Offered: true, Query: query}
	if query == "" {
		return search, store.Where{}
	}

	place, remembered, err := s.store.CachedPlace(query)
	if err != nil {
		log.Printf("web: reading a remembered place: %v", err)
	}
	if !remembered {
		place, err = s.places.Find(ctx, query)
		if err != nil {
			log.Printf("web: looking up a place: %v", err)
			search.Unavailable = true
			return search, store.Where{}
		}
		if err := s.store.RememberPlace(place); err != nil {
			log.Printf("web: remembering a place: %v", err)
		}
	}

	search.Name, search.Known = place.Name, place.Found
	if !place.Found {
		return search, store.Where{}
	}
	area := place.Area
	return search, store.Where{Within: &area}
}

// rememberedWhere is the narrowing for a window of cells: the page already looked the name
// up, so a window that arrives a minute later reads the answer and asks nobody.
func (s *Server) rememberedWhere(query string) store.Where {
	if query == "" {
		return store.Where{}
	}
	place, remembered, err := s.store.CachedPlace(query)
	if err != nil || !remembered || !place.Found {
		return store.Where{}
	}
	area := place.Area
	return store.Where{Within: &area}
}
