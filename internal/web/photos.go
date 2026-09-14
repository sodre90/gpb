package web

import (
	"log"
	"net/http"
	"net/url"

	"gpb/internal/store"
)

// photosView is every photo the account has, in one place. The album pages answer "what is in
// this album"; on a real library most photos are in no album at all — 52,044 of 96,388 rows on
// the deployed box belong to the library walk and the rest are scattered across 181 albums — so
// without this there is no way to look at the collection as a collection.
//
// It deliberately does not pick. Picking is a per-album decision, an item can sit in several
// albums at once, and a tick here could not say which of them it meant.
type photosView struct {
	Items    []itemCell
	Total    int
	Page     int
	Pages    int
	PrevPage int
	NextPage int
	BackedUp int
	Timeline timeline
	Search   placeSearch
	// Located is how far the sweep reading each file for its place has got. Until it has
	// finished, a search is over part of the library, and the page has to say so.
	Located store.LocationProgress
}

func (s *Server) handlePhotos(w http.ResponseWriter, r *http.Request) {
	search, where := s.searchFor(r.Context(), placeQuery(r))
	view, err := s.photosView(pageNumber(r), search, where)
	if err == nil && search.Query != "" && !search.Found() {
		// A name that found nothing narrows the grid to nothing, not to everything.
		view = photosView{Search: search, Located: view.Located}
	}
	if err != nil {
		log.Printf("web: building the photo grid: %v", err)
		http.Error(w, "the photos are unavailable", http.StatusInternalServerError)
		return
	}

	data := s.page(r, "Photos")
	data.Data = view
	render(w, http.StatusOK, "photos", data)
}

func (s *Server) photosView(page int, search placeSearch, where store.Where) (photosView, error) {
	total, err := s.store.EveryItemCount(where)
	if err != nil {
		return photosView{}, err
	}

	pages := pagesFor(total)
	page = min(max(page, 1), pages)

	items, err := s.store.EveryItemPage(where, (page-1)*pageSize, pageSize)
	if err != nil {
		return photosView{}, err
	}

	set, err := s.store.BackupSet()
	if err != nil {
		return photosView{}, err
	}
	months, err := s.store.EveryItemMonths(where)
	if err != nil {
		return photosView{}, err
	}
	timeline, err := timelineFor(cellsURLFor(search), months, total, page)
	if err != nil {
		return photosView{}, err
	}
	located, err := s.store.LocationProgress()
	if err != nil {
		return photosView{}, err
	}

	view := photosView{
		Total: total, BackedUp: set.Done,
		Page: page, Pages: pages, PrevPage: page - 1, NextPage: page + 1,
		Items:    make([]itemCell, 0, len(items)),
		Timeline: timeline,
		Search:   search,
		Located:  located,
	}
	for _, item := range items {
		view.Items = append(view.Items, cellFor(item, false))
	}
	return view, nil
}

// cellsURLFor is where the timeline fetches its windows from: the same grid, narrowed the same
// way, or the fetched cells would show the whole library under a heading naming one place.
func cellsURLFor(search placeSearch) string {
	if !search.Found() {
		return "/photos/cells"
	}
	return "/photos/cells?place=" + url.QueryEscape(search.Query)
}
