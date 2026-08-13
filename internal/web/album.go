package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"gpb/internal/auth"
	"gpb/internal/store"
	"gpb/internal/syncer"
)

// pageSize is how many cells one grid page holds. Big enough that a typical album is one or
// two pages, small enough that the browser is not decoding a thousand images at once.
const pageSize = 200

// maxKeysPerRequest bounds one request that names the items it acts on — picking from the grid,
// or settling the review queue. Either sends at most what a page holds, which on a large one is
// a few hundred keys; anything far beyond that is not a person clicking.
const maxKeysPerRequest = 2000

type albumView struct {
	ID    string
	Title string
	Mode  store.SyncMode
	Items []itemCell
	// Total counts the items this store knows about; UpstreamTotal is how many Google said the
	// album holds when it was last listed. They differ until the album has been walked, which
	// is what tells the page whether an empty grid means "not looked yet" or "really empty".
	Total         int
	UpstreamTotal int
	Kind          store.AlbumKind
	Owner         string
	OwnedByMe     bool
	Picked        int
	BackedUp      int
	Page          int
	Pages         int
	PrevPage      int
	NextPage      int
	Picking       bool
	Activity      string
	Running       bool
}

type itemCell struct {
	MediaKey   string
	Filename   string
	CapturedAt string
	State      store.State
	IsVideo    bool
	Picked     bool
	Missing    bool
	Failed     bool
	Downloaded bool
	Review     bool
	Size       string
}

// gridCell is a cell and the name of the form field it posts under. The same cell appears in
// the album grid and in the review queue, which mean different things by a tick and so name the
// field differently; everything else about the cell is identical, and is written once. An empty
// field means the grid decides nothing — the photo page only shows — so no checkbox is drawn.
type gridCell struct {
	Cell  itemCell
	Field string
}

func (c itemCell) In(field string) gridCell { return gridCell{Cell: c, Field: field} }

func (c itemCell) Shown() gridCell { return gridCell{Cell: c} }

func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
	view, err := s.albumView(r.PathValue("id"), pageNumber(r))
	if err != nil {
		log.Printf("web: building the item grid: %v", err)
		http.Error(w, "that album is unavailable", http.StatusNotFound)
		return
	}

	if s.listOnFirstView(view) {
		view.Activity, view.Running = s.runs.Activity(), true
	}

	data := s.page(r, view.DisplayTitle())
	data.Data = view
	render(w, http.StatusOK, "album", data)
}

// listOnFirstView walks an album the first time someone opens it, so that deciding whether to
// back an album up does not require backing it up to see inside. It matters most for "picked":
// an album nobody has listed offers nothing to pick.
//
// Three things stop it from firing repeatedly. A stored row means the album has been walked
// already. Google's own count of zero means there is nothing to walk. And a run in flight,
// including the one this just started, owns the profile — so the page's own polling cannot
// stack listings on top of each other.
func (s *Server) listOnFirstView(view albumView) bool {
	if !shouldListOnFirstView(view, s.auth.Status().State) {
		return false
	}

	if err := s.runs.StartAlbumListing(view.ID, "album opened"); err != nil {
		if !errors.Is(err, syncer.ErrRunInProgress) && !errors.Is(err, auth.ErrProfileBusy) {
			log.Printf("web: listing an album on first view: %v", err)
		}
		return false
	}
	return true
}

// shouldListOnFirstView is separated from the run it starts so the rules can be read, and
// tested, without a runner or a browser behind them. A session needing re-auth is a rule and
// not an optimisation: without it every album the user clicked through would launch a warmup
// that was always going to fail, and none of the grids would fill anyway.
func shouldListOnFirstView(view albumView, state auth.State) bool {
	return view.Total == 0 &&
		view.UpstreamTotal > 0 &&
		!view.Running &&
		state != auth.StateAuthRequired
}

func (s *Server) albumView(albumID string, page int) (albumView, error) {
	album, err := s.store.Album(albumID)
	if err != nil {
		return albumView{}, err
	}
	total, err := s.store.AlbumItemCount(albumID)
	if err != nil {
		return albumView{}, err
	}

	pages := pagesFor(total)
	page = min(max(page, 1), pages)

	items, err := s.store.AlbumPage(albumID, (page-1)*pageSize, pageSize)
	if err != nil {
		return albumView{}, err
	}
	picked, err := s.store.SelectionIn(albumID)
	if err != nil {
		return albumView{}, err
	}
	stats, err := s.store.AlbumStats(albumID)
	if err != nil {
		return albumView{}, err
	}

	view := albumView{
		ID: album.ID, Title: album.Title, Mode: album.SyncMode,
		Total: total, UpstreamTotal: album.ItemCount, Kind: album.Kind,
		Owner: album.OwnerName, OwnedByMe: album.OwnerIsAccount,
		Picked: len(picked), BackedUp: stats.Done, Page: page, Pages: pages,
		PrevPage: page - 1, NextPage: page + 1,
		Picking:  album.SyncMode == store.SyncPicked,
		Activity: s.runs.Activity(),
		Items:    make([]itemCell, 0, len(items)),
	}
	view.Running = view.Activity != ""
	for _, item := range items {
		view.Items = append(view.Items, cellFor(item, picked[item.MediaKey]))
	}
	return view, nil
}

func cellFor(item store.MediaItem, picked bool) itemCell {
	return itemCell{
		MediaKey:   item.MediaKey,
		Filename:   item.Filename,
		CapturedAt: humanDate(item.CapturedAt),
		State:      item.State,
		IsVideo:    item.IsVideo,
		Picked:     picked,
		Missing:    item.State == store.StateMissingUpstream,
		Failed:     item.State == store.StateFailed,
		Downloaded: item.State == store.StateDone,
		Review:     item.NeedsReview,
		Size:       humanBytes(item.SizeBytes),
	}
}

// pickRequest is what the grid posts. Keys arrive as a batch because a shift-click range is
// one user action and should be one request — and one transaction.
type pickRequest struct {
	MediaKeys []string `json:"mediaKeys"`
	Selected  bool     `json:"selected"`
	Scope     string   `json:"scope"`
}

type pickResponse struct {
	Picked int `json:"picked"`
	Total  int `json:"total"`
}

// handlePicks records a selection change and answers with the album's new totals, so the
// header count stays right without the page reloading. A form arrives here too — the Save picks
// button a browser with no script still has — and is answered by going back to the grid.
func (s *Server) handlePicks(w http.ResponseWriter, r *http.Request) {
	albumID := r.PathValue("id")
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		s.savePickedPage(w, r, albumID)
		return
	}

	var request pickRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}

	if err := s.applyPicks(albumID, request); err != nil {
		log.Printf("web: applying picks: %v", err)
		http.Error(w, "the selection could not be saved", http.StatusInternalServerError)
		return
	}

	picked, err := s.store.SelectionIn(albumID)
	if err != nil {
		log.Printf("web: re-reading the selection: %v", err)
		http.Error(w, "the selection could not be read back", http.StatusInternalServerError)
		return
	}
	total, err := s.store.AlbumItemCount(albumID)
	if err != nil {
		log.Printf("web: counting album items: %v", err)
		http.Error(w, "the selection could not be read back", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pickResponse{Picked: len(picked), Total: total})
}

// savePickedPage is the no-script path: a form of checkboxes, of which the browser sends only
// the ticked ones. What was unticked is therefore not in the request at all, and can only be
// known from the page's own account of what it drew, which the form carries as hidden fields.
//
// Working that out at save time instead — re-reading the page from the store — is what let a tab
// left open all afternoon clear a pick approved in the review queue since it rendered: the item
// was on the page the server re-read, and absent from the form the browser sent. Every key is
// still checked against the album, so a rewritten form cannot reach into one the user was not
// looking at.
func (s *Server) savePickedPage(w http.ResponseWriter, r *http.Request, albumID string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}

	picked, cleared, err := s.pickedOnThePage(albumID, r.Form["shown"], r.Form["pick"])
	if err != nil {
		log.Printf("web: reading what a grid page held: %v", err)
		s.backToGrid(w, r, "error", "The picks could not be saved.")
		return
	}

	if err := s.store.SetSelection(picked, true); err != nil {
		log.Printf("web: saving picks from a form: %v", err)
		s.backToGrid(w, r, "error", "The picks could not be saved.")
		return
	}
	if err := s.store.SetSelection(cleared, false); err != nil {
		log.Printf("web: clearing picks from a form: %v", err)
		s.backToGrid(w, r, "error", "The picks could not be saved.")
		return
	}
	s.backToGrid(w, r, "notice", quantity(len(picked), "item")+" picked on this page.")
}

// pickedOnThePage splits what the page drew into what the user left ticked and what they did
// not. A key the form ticked without also reporting it as drawn is still honoured — an older
// page, open since before the form said what it held, should save the picks somebody made on it
// rather than silently drop them — but it clears nothing, because that page cannot speak for
// what it never listed.
func (s *Server) pickedOnThePage(albumID string, shown, ticked []string) (picked, cleared []string, err error) {
	named := slices.Concat(shown, ticked)
	if len(named) > maxKeysPerRequest {
		return nil, nil, fmt.Errorf("a picks form carried %d keys", len(named))
	}

	onThePage, err := s.store.MediaKeysIn(albumID, named)
	if err != nil {
		return nil, nil, err
	}

	wanted := make(map[string]bool, len(ticked))
	for _, key := range ticked {
		wanted[key] = true
	}
	for _, key := range onThePage {
		if wanted[key] {
			picked = append(picked, key)
			continue
		}
		cleared = append(cleared, key)
	}
	return picked, cleared, nil
}

// applyPicks refuses keys that do not belong to the album being viewed. The browser sends
// keys it rendered, but a request is not evidence of what was rendered, and marking items in
// an album the user was not looking at would be a silent surprise at the next sync.
func (s *Server) applyPicks(albumID string, request pickRequest) error {
	if request.Scope == "album" {
		return s.store.SelectWholeAlbum(albumID, request.Selected)
	}
	if len(request.MediaKeys) > maxKeysPerRequest {
		return fmt.Errorf("a picks request carried %d keys", len(request.MediaKeys))
	}

	belonging, err := s.store.MediaKeysIn(albumID, request.MediaKeys)
	if err != nil {
		return err
	}
	if len(belonging) != len(request.MediaKeys) {
		log.Printf("web: a picks request named %d keys, %d of them in the album",
			len(request.MediaKeys), len(belonging))
	}
	return s.store.SetSelection(belonging, request.Selected)
}

// handleAlbumModeFromGrid changes the mode from the grid's own header, then comes back to
// the grid rather than to the album list — the user is in the middle of curating this album.
func (s *Server) handleAlbumModeFromGrid(w http.ResponseWriter, r *http.Request) {
	albumID := r.PathValue("id")
	mode := store.SyncMode(r.FormValue("mode"))

	if err := s.store.SetAlbumSyncMode(albumID, mode); err != nil {
		log.Printf("web: setting a sync mode from the grid: %v", err)
		s.backToGrid(w, r, "error", "That album could not be updated.")
		return
	}

	s.relinkAlbums()
	s.backToGrid(w, r, "notice", noticeFor(mode))
}

// handleAlbumItemRefresh asks Google for this one album's contents. Without it an album
// switched to "picked" shows an empty grid: nothing walks an album's items until a sync run
// visits it, and a full sync is a long wait for the answer to "what is in here?".
func (s *Server) handleAlbumItemRefresh(w http.ResponseWriter, r *http.Request) {
	albumID := r.PathValue("id")

	switch err := s.runs.StartAlbumListing(albumID, "web ui"); {
	case err == nil:
		s.backToGrid(w, r, "notice", "Fetching this album's contents from Google.")
	case errors.Is(err, syncer.ErrRunInProgress), errors.Is(err, auth.ErrProfileBusy):
		s.backToGrid(w, r, "error", "Something is already running; give it a moment.")
	default:
		log.Printf("web: listing an album: %v", err)
		s.backToGrid(w, r, "error", "Could not start: "+err.Error())
	}
}

func (s *Server) backToGrid(w http.ResponseWriter, r *http.Request, kind, message string) {
	http.Redirect(w, r, albumURL(r.PathValue("id"), pageNumber(r))+
		"&"+kind+"="+url.QueryEscape(message), http.StatusSeeOther)
}

func albumURL(albumID string, page int) string {
	return "/album/" + url.PathEscape(albumID) + "?page=" + strconv.Itoa(page)
}

// pagesFor never answers zero: an empty grid is still one page, and a pager that says
// "page 1 of 0" is a bug the user has to read.
func pagesFor(total int) int {
	return max(1, (total+pageSize-1)/pageSize)
}

func pageNumber(r *http.Request) int {
	page, err := strconv.Atoi(r.FormValue("page"))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

func (v albumView) DisplayTitle() string {
	return displayTitle(v.Title, v.Kind, v.Owner, v.OwnedByMe)
}

// Listing is the one state this page refreshes for: a grid with nothing in it yet, waiting on
// the walk that fills it. Once there are cells there is a selection to protect, and reloading
// under it costs more than the moving counts are worth.
func (v albumView) Listing() bool { return v.Running && len(v.Items) == 0 }

// Badge marks an album that is someone else's to change underneath the user.
func (v albumView) Badge() string {
	if v.Kind == store.AlbumShared {
		return "shared with you"
	}
	return ""
}

// Placeholders stands in for the cells a walk has not produced yet. The count is a picture of
// waiting rather than a prediction — nothing knows how many items the album holds until Google
// has been asked — and one screenful of them is enough to say "this is loading".
func (v albumView) Placeholders() []struct{} { return make([]struct{}, skeletonCells) }

const skeletonCells = 12
