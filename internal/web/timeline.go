package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"gpb/internal/store"
)

// timeline is what a grid page hands its script so the script can lay the whole grid out
// before it has fetched any of it: the months in grid order with their counts, and where to
// fetch a window of cells from. The script's own scrollbar then spans the whole library, and a
// drag along the date rail lands on a month without a page number in between.
//
// Months carries the JSON the grid element embeds. It is marshalled here rather than in the
// template because html/template has no opinion about JSON, and a data attribute of it is the
// one place a page can hand a script a value without a second request.
type timeline struct {
	Cells  string
	Months string
	Total  int
	// Offset is the index of the first cell the page rendered itself, so the script can keep
	// that page as the first window it holds rather than fetching it again; Window is how many
	// cells a window holds, which is the page size, so the rendered page is exactly one.
	Offset int
	Window int
}

type monthBand struct {
	Month string `json:"month"`
	Count int    `json:"count"`
}

func timelineFor(cellsURL string, months []store.MonthCount, total, page int) (timeline, error) {
	bands := make([]monthBand, 0, len(months))
	for _, month := range months {
		bands = append(bands, monthBand(month))
	}
	encoded, err := json.Marshal(bands)
	if err != nil {
		return timeline{}, err
	}
	return timeline{Cells: cellsURL, Months: string(encoded), Total: total,
		Offset: (page - 1) * pageSize, Window: pageSize}, nil
}

// cellsWindow is what a cells request asks for. A window is at most a page, which is what the
// store is tuned to answer and what a scroll stop needs; a script wanting more asks again.
func cellsWindow(r *http.Request) (offset, limit int) {
	offset, _ = strconv.Atoi(r.FormValue("offset"))
	limit, _ = strconv.Atoi(r.FormValue("limit"))
	return max(offset, 0), min(max(limit, 1), pageSize)
}

// cellsFragment is a run of grid cells on their own, rendered through the same block as the
// page so a cell the script placed cannot differ from a cell a reload would have drawn.
type cellsFragment struct {
	Cells []gridCell
}

// handlePhotoCells serves a window of the whole-library grid.
func (s *Server) handlePhotoCells(w http.ResponseWriter, r *http.Request) {
	offset, limit := cellsWindow(r)
	items, err := s.store.EveryItemPage(offset, limit)
	if err != nil {
		log.Printf("web: reading a window of the photo grid: %v", err)
		http.Error(w, "the photos are unavailable", http.StatusInternalServerError)
		return
	}

	fragment := cellsFragment{Cells: make([]gridCell, 0, len(items))}
	for _, item := range items {
		fragment.Cells = append(fragment.Cells, cellFor(item, false).Shown())
	}
	writeFragment(w, "photos", "gridcells", fragment)
}

// handleAlbumCells serves a window of one album's grid, with the checkbox the album's mode
// calls for, ticked as the store has it right now.
func (s *Server) handleAlbumCells(w http.ResponseWriter, r *http.Request) {
	albumID := r.PathValue("id")
	album, err := s.store.Album(albumID)
	if err != nil {
		http.Error(w, "that album is unavailable", http.StatusNotFound)
		return
	}

	offset, limit := cellsWindow(r)
	items, err := s.store.AlbumPage(albumID, offset, limit)
	if err != nil {
		log.Printf("web: reading a window of an album grid: %v", err)
		http.Error(w, "that album is unavailable", http.StatusInternalServerError)
		return
	}
	picked, err := s.store.SelectionIn(albumID)
	if err != nil {
		log.Printf("web: reading an album's selection: %v", err)
		http.Error(w, "that album is unavailable", http.StatusInternalServerError)
		return
	}

	fragment := cellsFragment{Cells: make([]gridCell, 0, len(items))}
	for _, item := range items {
		cell := cellFor(item, picked[item.MediaKey])
		if album.SyncMode == store.SyncPicked {
			fragment.Cells = append(fragment.Cells, cell.In("pick"))
		} else {
			fragment.Cells = append(fragment.Cells, cell.Shown())
		}
	}
	writeFragment(w, "album", "gridcells", fragment)
}
