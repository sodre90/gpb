package web

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"gpb/internal/auth"
	"gpb/internal/links"
	"gpb/internal/store"
	"gpb/internal/syncer"
)

type albumsView struct {
	Albums []albumRow
	// Bundles are the sets of loose shared photos, kept out of Albums because they are not
	// albums and there are forty-nine of them: listed together with the real ones they are a
	// wall of near-identical nameless rows between the albums the user came here for.
	Bundles []albumRow
	// BundlesOpen and BundlesLink are the section's disclosure, driven by the URL rather than
	// by the browser so that sorting it — or saving a row in it — does not close it.
	BundlesOpen bool
	BundlesLink string
	// FavouritesOnly and FavouritesLink are the filter header's own control, driven by the URL
	// for the same reason the disclosure is: sorting reloads this page.
	FavouritesOnly bool
	FavouritesLink string
	// Favourites counts the starred albums whether or not the filter is on, so the control can
	// say how many rows it would leave and a filter showing nothing can explain itself.
	Favourites int
	// Library is everything the account holds, which is mostly photos in no album at all. It is
	// its own card rather than a row in the table because it is the only entry that carries a
	// date, and because "back up everything" deserves to be read before it is clicked.
	Library libraryCard
	Columns []albumColumn
	// Query is the current order as a URL suffix, for the forms on this page to post back
	// through. Without it every "Save" would drop the user back into the default order.
	Query    string
	Followed int
	// Now is the run in flight, reported here as a banner rather than a self-refreshing page.
	// This is the one screen made of open selectors, and a page that reloads itself under a
	// half-made choice loses the choice.
	Now     *nowCard
	Running bool
}

// albumTally is the line under the table. It is a template block of its own so that a row saved
// without reloading the page can bring the recount with it, rather than the sentence being
// written out a second time in script.
type albumTally struct {
	Count    int
	Followed int
}

func (v albumsView) Tally() albumTally {
	return albumTally{Count: len(v.Albums), Followed: v.Followed}
}

type albumRow struct {
	ID        string
	Title     string
	ItemCount int
	Mode      store.SyncMode
	Created   string
	CreatedAt time.Time
	Kind      store.AlbumKind
	HasCover  bool
	Owner     string
	OwnedByMe bool
	Favourite bool
	// Query is the current sort, carried per row so the mode form on it can post back without
	// throwing away the arrangement the user set up to find the album.
	Query       string
	Done        int
	Pending     int
	Failed      int
	Missing     int
	NeedsReview int
	Picked      int
	LastSynced  string
}

// libraryCard is the whole-account row. Everything on it except Since is a count of what has
// already happened: a library walk is long enough that the page has to show it moving.
type libraryCard struct {
	Mode store.SyncMode
	// Since is a yyyy-mm-dd string because that is what an <input type="date"> reads and writes.
	// Empty means no bound, which means every photo the account has ever held.
	Since      string
	Known      int
	Done       int
	Pending    int
	Failed     int
	LastSynced string
	Query      string
}

func (l libraryCard) Following() bool { return l.Mode != store.SyncNone }

// albumColumn is a table heading the user can click. The link, the arrow and the sort state are
// worked out here rather than in the template so the template holds no rules about what sorting
// means. Class is what the narrow-screen rules hide the column by: a heading and its cells have
// to disappear together, and only the server knows which heading is which.
type albumColumn struct {
	Label     string
	Link      string
	Indicator string
	Numeric   bool
	Class     string
	// Sort is the aria-sort value, which is empty on every column but the one in force — the
	// attribute means "this table is arranged by me", and three of them would be a lie.
	Sort string
}

// placeholderTitle names an entry Google gave no title for. A bundle of shared photos has no
// name in any response Google serves this account — not the listing, not its contents, not its
// own page, which Google itself heads "Shared photos" — so calling it untitled blames the user
// for something they never did, and 49 of this library's entries are bundles.
//
// Naming whoever the photos came from is the only thing that distinguishes one such row from
// the next, and saying "you" where it applies is the difference between a set the user sent
// out and one that arrived.
func placeholderTitle(kind store.AlbumKind, owner string, ownedByMe bool) string {
	if kind != store.AlbumBundle {
		return "Untitled album"
	}
	switch {
	case ownedByMe:
		return "Photos you shared"
	case owner != "":
		return "Shared photos from " + owner
	default:
		return "Shared photos"
	}
}

func displayTitle(title string, kind store.AlbumKind, owner string, ownedByMe bool) string {
	if title != "" {
		return title
	}
	return placeholderTitle(kind, owner, ownedByMe)
}

func (a albumRow) DisplayTitle() string {
	return displayTitle(a.Title, a.Kind, a.Owner, a.OwnedByMe)
}

// Badge marks an album that is someone else's to change underneath the user. Bundles carry no
// badge: they have a section of their own, and their title already says what they are.
func (a albumRow) Badge() string {
	if a.Kind == store.AlbumShared {
		return "shared with you"
	}
	return ""
}

// NothingPicked is the trap this row exists to make visible: an album set to "picked" with
// no picks looks followed and backs up nothing at all.
func (a albumRow) NothingPicked() bool {
	return a.Mode == store.SyncPicked && a.Picked == 0
}

func (s *Server) handleAlbums(w http.ResponseWriter, r *http.Request) {
	s.renderAlbums(w, r, http.StatusOK, "")
}

// handleAlbumMode is the picker. It is a plain form POST per row, so the page works with no
// JavaScript at all — which matters for the one screen the whole product is configured from.
// The page's own script posts the same form and asks for the changed row back instead of a
// redirect, so choosing a mode on a list of two hundred does not reload it.
func (s *Server) handleAlbumMode(w http.ResponseWriter, r *http.Request) {
	albumID := r.PathValue("id")
	mode := store.SyncMode(r.FormValue("mode"))
	inPlace := savingInPlace(r)

	if err := s.store.SetAlbumSyncMode(albumID, mode); err != nil {
		log.Printf("web: setting a sync mode: %v", err)
		if inPlace {
			http.Error(w, "that album could not be updated", http.StatusBadRequest)
			return
		}
		s.renderAlbums(w, r, http.StatusBadRequest, "That album could not be updated.")
		return
	}

	s.relinkAlbums()
	if inPlace {
		s.writeSavedRow(w, r, albumID)
		return
	}
	redirectWithNotice(w, r, noticeFor(mode))
}

// handleAlbumFavourite is the star, and like the picker beside it a plain form POST per row, so
// this page still works with no script at all. What is posted is the state wanted rather than an
// instruction to flip: two clicks racing each other then agree on an answer instead of undoing
// one another.
func (s *Server) handleAlbumFavourite(w http.ResponseWriter, r *http.Request) {
	albumID := r.PathValue("id")
	favourite := r.FormValue("favourite") == wantFavourite
	inPlace := savingInPlace(r)

	if err := s.store.SetAlbumFavourite(albumID, favourite); err != nil {
		log.Printf("web: starring an album: %v", err)
		if inPlace {
			http.Error(w, "that album could not be updated", http.StatusBadRequest)
			return
		}
		s.renderAlbums(w, r, http.StatusBadRequest, "That album could not be updated.")
		return
	}

	if inPlace {
		s.writeSavedRow(w, r, albumID)
		return
	}
	redirectWithNotice(w, r, favouriteNotice(favourite))
}

const wantFavourite = "yes"

func favouriteNotice(favourite bool) string {
	if favourite {
		return "Album starred: it leads the list, and the next run fetches it first."
	}
	return "Album is no longer a favourite."
}

// savingInPlace tells a save made by the page's own script from one made by submitting the
// form. The first wants the row back; the second wants to be sent somewhere.
func savingInPlace(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// savedRow is what the selector gets back: the row as the server would have rendered it, and
// the tally beneath the table, which counts rows this response does not carry.
type savedRow struct {
	Row   string `json:"row"`
	Tally string `json:"tally"`
	// Gone says the album is no longer one this page shows, which is what unstarring a row means
	// while the list is filtered to favourites. Without it the script would put back a row the
	// filter had just excluded.
	Gone bool `json:"gone"`
}

// writeSavedRow builds the list afresh rather than patching the row the request arrived on. A
// mode change moves everything beside it — an album backing up nothing has no progress to show,
// one set to "picked" with nothing picked earns a badge — and rebuilding is what keeps the row
// that lands in the page identical to the row a reload would have brought.
func (s *Server) writeSavedRow(w http.ResponseWriter, r *http.Request, albumID string) {
	view, err := s.albumsView(queryFrom(r))
	if err != nil {
		log.Printf("web: rebuilding the album list: %v", err)
		http.Error(w, "the album could not be read back", http.StatusInternalServerError)
		return
	}

	saved := savedRow{Gone: true}
	switch row, shown := view.row(albumID); {
	case shown:
		if saved.Row, err = renderBlock("albums", "albumrow", row); err != nil {
			log.Printf("web: rendering an album row: %v", err)
			http.Error(w, "the album could not be rendered", http.StatusInternalServerError)
			return
		}
		saved.Gone = false
	case !view.FavouritesOnly:
		http.Error(w, "no such album", http.StatusNotFound)
		return
	}

	renderedTally, err := renderBlock("albums", "albumtally", view.Tally())
	if err != nil {
		log.Printf("web: rendering the album tally: %v", err)
		http.Error(w, "the album could not be rendered", http.StatusInternalServerError)
		return
	}
	saved.Tally = renderedTally

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(saved)
}

func (v albumsView) row(albumID string) (albumRow, bool) {
	for _, row := range slices.Concat(v.Albums, v.Bundles) {
		if row.ID == albumID {
			return row, true
		}
	}
	return albumRow{}, false
}

func noticeFor(mode store.SyncMode) string {
	switch mode {
	case store.SyncAll:
		return "Album will be backed up in full."
	case store.SyncPicked:
		return "Album will back up only picked items."
	default:
		return "Album will no longer be backed up."
	}
}

// redirectWithNotice sends the browser back to the list rather than rendering in place, so a
// reload after a change does not resubmit it. The sort travels with it: a user who ordered the
// list to find an album is still looking for it after changing its mode.
func redirectWithNotice(w http.ResponseWriter, r *http.Request, notice string) {
	query := queryFrom(r).keep(url.Values{"notice": {notice}})
	http.Redirect(w, r, albumsPath+"?"+query.Encode(), http.StatusSeeOther)
}

const albumsPath = "/albums"

// relinkAlbums keeps the browsable album view in step with the selector the user just
// changed — an unfollowed album's folder should disappear when they say so, not at the next
// sync. A failure is logged, never surfaced: the setting was saved, which is what was asked.
func (s *Server) relinkAlbums() {
	if _, err := links.Rebuild(s.store, s.cfg.PhotosDir); err != nil {
		log.Printf("web: rebuilding the album view: %v", err)
	}
}

func (s *Server) handleAlbumRefresh(w http.ResponseWriter, r *http.Request) {
	s.startRun(w, r, s.runs.StartRefresh, s.renderAlbums, func() {
		redirectWithNotice(w, r, "Fetching the album list from Google.")
	})
}

// handleSyncNow answers on the Activity page whatever happens, because that is where the thing
// it started can be watched. The button exists on two pages; sending the user back to whichever
// one they pressed it on would leave them looking at a list while the run they asked for is
// reported somewhere else.
func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	s.startRun(w, r, s.runs.StartSync, s.renderRuns, func() {
		http.Redirect(w, r, "/runs?notice=Backup+started.", http.StatusSeeOther)
	})
}

// startRun reports "already running" as a conflict rather than an error: asking twice is a
// reasonable thing for a person to do when the first click produced no visible change.
func (s *Server) startRun(w http.ResponseWriter, r *http.Request, start func(string) error,
	refuse problemRenderer, accept func()) {

	switch err := start("web ui"); {
	case err == nil:
		accept()
	case errors.Is(err, syncer.ErrRunInProgress), errors.Is(err, auth.ErrProfileBusy):
		refuse(w, r, http.StatusConflict, "Something is already running; give it a moment.")
	default:
		log.Printf("web: starting a run: %v", err)
		refuse(w, r, http.StatusBadGateway, "Could not start: "+err.Error())
	}
}

// problemRenderer re-renders the page a failed action was asked from, with the reason on it.
// The page is the caller's choice because a refusal belongs where the button was.
type problemRenderer func(w http.ResponseWriter, r *http.Request, code int, problem string)

func (s *Server) renderAlbums(w http.ResponseWriter, r *http.Request, code int, problem string) {
	view, err := s.albumsView(queryFrom(r))
	if err != nil {
		log.Printf("web: building the album list: %v", err)
		http.Error(w, "the album list is unavailable", http.StatusInternalServerError)
		return
	}

	data := s.page(r, "Albums")
	if problem != "" {
		data.Error = problem
	}
	data.Data = view
	render(w, code, "albums", data)
}

func (s *Server) albumsView(order albumsQuery) (albumsView, error) {
	albums, err := s.store.Albums()
	if err != nil {
		return albumsView{}, err
	}
	stats, err := s.store.AlbumStatsByID()
	if err != nil {
		return albumsView{}, err
	}

	view := albumsView{Albums: make([]albumRow, 0, len(albums)), Now: s.nowCard()}
	view.Running = view.Now != nil
	view.Query = order.suffix()
	view.BundlesOpen = order.BundlesOpen
	view.BundlesLink = order.toggleBundlesLink()
	view.FavouritesOnly = order.FavouritesOnly
	view.FavouritesLink = order.toggleFavouritesLink()

	for _, album := range albums {
		if album.Kind == store.AlbumLibrary {
			view.Library = libraryCardFor(album, stats[album.ID], view.Query)
			continue
		}
		if album.Favourite {
			view.Favourites++
		}
		if order.FavouritesOnly && !album.Favourite {
			continue
		}
		if album.SyncMode != store.SyncNone {
			view.Followed++
		}
		row := rowFor(album, stats[album.ID], view.Query)
		if album.Kind == store.AlbumBundle {
			view.Bundles = append(view.Bundles, row)
			continue
		}
		view.Albums = append(view.Albums, row)
	}
	order.sort(view.Albums)
	order.sort(view.Bundles)
	view.Columns = order.headings()
	return view, nil
}

func rowFor(album store.Album, stats store.AlbumStats, query string) albumRow {
	return albumRow{
		ID:          album.ID,
		Title:       album.Title,
		ItemCount:   album.ItemCount,
		Mode:        album.SyncMode,
		Created:     humanDate(album.CreatedAt),
		CreatedAt:   album.CreatedAt,
		Kind:        album.Kind,
		HasCover:    album.CoverURL != "",
		Owner:       album.OwnerName,
		OwnedByMe:   album.OwnerIsAccount,
		Favourite:   album.Favourite,
		Query:       query,
		Done:        stats.Done,
		Pending:     stats.Pending,
		Failed:      stats.Failed,
		Missing:     stats.Missing,
		NeedsReview: stats.NeedsReview,
		Picked:      stats.Picked,
		LastSynced:  humanTime(album.LastSyncedAt),
	}
}

func libraryCardFor(album store.Album, stats store.AlbumStats, query string) libraryCard {
	card := libraryCard{
		Mode:       album.SyncMode,
		Known:      stats.Known,
		Done:       stats.Done,
		Pending:    stats.Pending,
		Failed:     stats.Failed,
		LastSynced: humanTime(album.LastSyncedAt),
		Query:      query,
	}
	if !album.Since.IsZero() {
		card.Since = album.Since.Format(sinceLayout)
	}
	return card
}

// sinceLayout is what <input type="date"> submits and expects back.
const sinceLayout = "2006-01-02"

// handleLibrary saves the whole-account instruction. Mode and date arrive together because
// they are one decision: saving "everything" while dropping the date the user typed beside it
// would start a walk through every photo they have ever taken.
func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	since, err := parseSince(r.FormValue("since"))
	if err != nil {
		s.renderAlbums(w, r, http.StatusBadRequest,
			"That date could not be read; use the date picker or yyyy-mm-dd.")
		return
	}

	mode := store.SyncMode(r.FormValue("mode"))
	if err := s.store.SetLibrary(mode, since); err != nil {
		log.Printf("web: setting the library instruction: %v", err)
		s.renderAlbums(w, r, http.StatusBadRequest, "The library could not be updated.")
		return
	}
	redirectWithNotice(w, r, libraryNotice(mode, since))
}

func parseSince(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(sinceLayout, value)
}

// libraryNotice says back what was actually saved. The date is the difference between a few
// hundred photos and a hundred thousand, so leaving it out of the confirmation would hide the
// one number that matters.
func libraryNotice(mode store.SyncMode, since time.Time) string {
	if mode == store.SyncNone {
		return "The library will no longer be backed up."
	}
	if since.IsZero() {
		return "The whole library will be backed up, as far back as it goes."
	}
	return "The library will be backed up from " + since.Format(sinceLayout) + " onwards."
}
