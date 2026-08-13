package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"gpb/internal/auth"
	"gpb/internal/store"
)

func seedItems(t *testing.T, server *Server, albumID string, count int) []string {
	t.Helper()

	now := time.Now()
	keys := make([]string, 0, count)
	for index := range count {
		key := fmt.Sprintf("AF1Qip%04d", index)
		item := store.MediaItem{
			MediaKey:     key,
			Filename:     fmt.Sprintf("IMG_%04d.HEIC", index),
			CapturedAt:   now.Add(time.Duration(index) * time.Minute),
			ThumbnailURL: "https://photos.fife.usercontent.google.com/pw/" + key,
		}
		if err := server.store.UpsertItem(item, now); err != nil {
			t.Fatalf("seeding item %s: %v", key, err)
		}
		if err := server.store.LinkItemToAlbum(albumID, key, now); err != nil {
			t.Fatalf("linking %s: %v", key, err)
		}
		keys = append(keys, key)
	}
	return keys
}

func postJSON(handler http.Handler, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		request.AddCookie(cookie)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// Ten thousand items deep the pager is the only control a reader has, so its labels have to say
// which way it really goes. An album grid runs oldest first — unlike every-photo, which runs
// newest first — and both pagers were labelled as though they ran the same way.
func TestTheAlbumPagerIsLabelledForTheOrderTheGridIsIn(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: pageSize + 1})
	seedItems(t, server, "holiday", pageSize+1)
	cookie := login(t, handler)

	first := get(handler, "/album/holiday", cookie).Body.String()
	second := get(handler, "/album/holiday?page=2", cookie).Body.String()

	oldest, newest := "IMG_0000.HEIC", fmt.Sprintf("IMG_%04d.HEIC", pageSize)
	switch {
	case !strings.Contains(first, oldest) || !strings.Contains(second, newest):
		t.Fatalf("the grid does not run from %s to %s, so this test is checking the wrong labels", oldest, newest)
	case !strings.Contains(first, `?page=2">Newer page`):
		t.Error("the forward link is not labelled as leading to newer photos, though it does")
	case !strings.Contains(second, `?page=1">← Older page`):
		t.Error("the back link is not labelled as leading to older photos, though it does")
	}
}

func TestTheGridShowsAnAlbumsItems(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 3})
	seedItems(t, server, "holiday", 3)

	recorder := get(handler, "/album/holiday", login(t, handler))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /album/holiday returned %d, want 200", recorder.Code)
	}

	body := recorder.Body.String()
	for _, want := range []string{"Holiday 2026", "/thumb/AF1Qip0000", `loading="lazy"`, "3 items"} {
		if !strings.Contains(body, want) {
			t.Errorf("the grid is missing %q", want)
		}
	}
}

// Picking is only meaningful in 'picked' mode. Offering the controls in the other modes
// would invite someone to curate an album whose every item is downloaded regardless.
func TestPickingToolsAppearOnlyInPickedMode(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 2)
	cookie := login(t, handler)

	if body := get(handler, "/album/holiday", cookie).Body.String(); strings.Contains(body, "Shift-click") {
		t.Error("an unfollowed album offered picking controls")
	}

	if err := server.store.SetAlbumSyncMode("holiday", store.SyncPicked); err != nil {
		t.Fatalf("switching to picked: %v", err)
	}
	if body := get(handler, "/album/holiday", cookie).Body.String(); !strings.Contains(body, "Shift-click") {
		t.Error("a picked album offered no picking controls")
	}
}

func TestPickingItemsRecordsTheSelection(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	keys := seedItems(t, server, "holiday", 5)
	cookie := login(t, handler)

	recorder := postJSON(handler, "/album/holiday/picks",
		pickRequest{MediaKeys: keys[:3], Selected: true}, cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("picking returned %d, want 200", recorder.Code)
	}

	var answer pickResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}
	if answer.Picked != 3 || answer.Total != 5 {
		t.Errorf("the answer reported %d of %d picked, want 3 of 5", answer.Picked, answer.Total)
	}

	selected, err := server.store.SelectionIn("holiday")
	if err != nil {
		t.Fatalf("reading the selection: %v", err)
	}
	if len(selected) != 3 {
		t.Errorf("the store holds %d picks, want 3", len(selected))
	}
}

func TestUnpickingRemovesTheSelection(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	keys := seedItems(t, server, "holiday", 3)
	cookie := login(t, handler)

	postJSON(handler, "/album/holiday/picks", pickRequest{MediaKeys: keys, Selected: true}, cookie)
	postJSON(handler, "/album/holiday/picks", pickRequest{MediaKeys: keys[:2], Selected: false}, cookie)

	selected, _ := server.store.SelectionIn("holiday")
	if len(selected) != 1 || !selected[keys[2]] {
		t.Errorf("the store holds %v, want only the last item picked", selected)
	}
}

// "Select all" runs server-side over the whole album. On a ten-thousand-item album the page
// has loaded 200 keys and must never need the rest just to tick a box.
func TestSelectAllCoversItemsTheBrowserNeverLoaded(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", pageSize+50)
	cookie := login(t, handler)

	recorder := postJSON(handler, "/album/holiday/picks",
		pickRequest{Scope: "album", Selected: true}, cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("select-all returned %d, want 200", recorder.Code)
	}

	selected, _ := server.store.SelectionIn("holiday")
	if len(selected) != pageSize+50 {
		t.Errorf("select-all marked %d items, want %d", len(selected), pageSize+50)
	}
}

// The browser posts keys it claims to have rendered. A request is not evidence of what was
// rendered, and marking items in an album the user was not looking at would surface months
// later as photos they never asked for.
func TestPicksCannotReachOutsideTheAlbum(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server,
		store.Album{ID: "holiday", Title: "Holiday 2026"},
		store.Album{ID: "private", Title: "Private"})
	holiday := seedItems(t, server, "holiday", 2)

	now := time.Now()
	secret := store.MediaItem{MediaKey: "AF1QipSECRET", Filename: "secret.HEIC"}
	if err := server.store.UpsertItem(secret, now); err != nil {
		t.Fatalf("seeding the other album's item: %v", err)
	}
	if err := server.store.LinkItemToAlbum("private", secret.MediaKey, now); err != nil {
		t.Fatalf("linking the other album's item: %v", err)
	}

	postJSON(handler, "/album/holiday/picks",
		pickRequest{MediaKeys: append(holiday, secret.MediaKey), Selected: true}, login(t, handler))

	other, _ := server.store.SelectionIn("private")
	if len(other) != 0 {
		t.Errorf("a picks request marked %d items in another album", len(other))
	}
	if mine, _ := server.store.SelectionIn("holiday"); len(mine) != 2 {
		t.Errorf("the album's own items were not picked: %v", mine)
	}
}

func TestTheGridPagesLargeAlbums(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", pageSize+10)
	cookie := login(t, handler)

	first := get(handler, "/album/holiday", cookie).Body.String()
	if strings.Count(first, `class="grid-cell`) != pageSize {
		t.Errorf("page one rendered %d cells, want %d", strings.Count(first, `class="grid-cell`), pageSize)
	}
	if !strings.Contains(first, "Page 1 of 2") {
		t.Error("page one does not say where it is in the album")
	}

	second := get(handler, "/album/holiday?page=2", cookie).Body.String()
	if strings.Count(second, `class="grid-cell`) != 10 {
		t.Errorf("page two rendered %d cells, want 10", strings.Count(second, `class="grid-cell`))
	}
}

// A page number out of range is a stale bookmark, not an error worth a 500.
func TestOutOfRangePagesLandSomewhereSensible(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 3)
	cookie := login(t, handler)

	for _, path := range []string{"/album/holiday?page=99", "/album/holiday?page=0", "/album/holiday?page=nonsense"} {
		recorder := get(handler, path, cookie)
		if recorder.Code != http.StatusOK {
			t.Errorf("%s returned %d, want 200", path, recorder.Code)
		}
	}
}

func TestAnUnknownAlbumIsNotFound(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	if recorder := get(handler, "/album/nope", login(t, handler)); recorder.Code != http.StatusNotFound {
		t.Errorf("an unknown album returned %d, want 404", recorder.Code)
	}
}

// Changing the mode from the grid should come back to the grid: the user is in the middle of
// curating this album, and being thrown back to the 181-row album list loses their place.
func TestSettingTheModeFromTheGridStaysOnTheGrid(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 3)

	recorder := postForm(handler, "/album/holiday/grid-mode",
		url.Values{"mode": {"picked"}, "page": {"1"}}, login(t, handler))

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("setting the mode returned %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); !strings.HasPrefix(location, "/album/holiday?page=1") {
		t.Errorf("redirected to %q, want back to the grid", location)
	}

	album, _ := server.store.Album("holiday")
	if album.SyncMode != store.SyncPicked {
		t.Errorf("the album is in %q mode", album.SyncMode)
	}
}

// stubImages stands in for Google so the serving path — store row, cache, headers — can be
// exercised without a session.
type stubImages struct {
	mu    sync.Mutex
	calls int
	urls  []string
	err   error
}

func (s *stubImages) Thumbnail(ctx context.Context, baseURL string, w io.Writer) (int64, error) {
	s.mu.Lock()
	s.calls++
	s.urls = append(s.urls, baseURL)
	s.mu.Unlock()

	if s.err != nil {
		return 0, s.err
	}
	written, err := io.WriteString(w, "jpeg-bytes")
	return int64(written), err
}

func TestAThumbnailIsServedAndThenCached(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 1)

	images := &stubImages{}
	server.images = images
	cookie := login(t, handler)

	for range 3 {
		recorder := get(handler, "/thumb/AF1Qip0000", cookie)
		if recorder.Code != http.StatusOK {
			t.Fatalf("a thumbnail returned %d, want 200", recorder.Code)
		}
		if recorder.Body.String() != "jpeg-bytes" {
			t.Fatalf("the handler served %q", recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); got != "image/jpeg" {
			t.Errorf("served as %q, want image/jpeg", got)
		}
		if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "private") {
			t.Errorf("cached as %q — this is one person's photo library", got)
		}
	}

	if images.calls != 1 {
		t.Errorf("three requests reached Google %d times, want 1", images.calls)
	}
	if want := "https://photos.fife.usercontent.google.com/pw/AF1Qip0000"; images.urls[0] != want {
		t.Errorf("fetched %q, want the URL from the listing", images.urls[0])
	}
}

// The cover is what makes a nameless bundle of shared photos recognisable at all, so it comes
// through the same cache as the grid: forty-nine of them on one page is forty-nine fetches.
func TestAnAlbumCoverIsServedAndThenCached(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026",
		CoverURL: "https://photos.fife.usercontent.google.com/pw/COVER"})

	images := &stubImages{}
	server.images = images
	cookie := login(t, handler)

	for range 3 {
		recorder := get(handler, "/album/holiday/cover", cookie)
		if recorder.Code != http.StatusOK {
			t.Fatalf("a cover returned %d, want 200", recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != "image/jpeg" {
			t.Errorf("served as %q, want image/jpeg", got)
		}
	}

	if images.calls != 1 {
		t.Errorf("three requests reached Google %d times, want 1", images.calls)
	}
	if want := "https://photos.fife.usercontent.google.com/pw/COVER"; images.urls[0] != want {
		t.Errorf("fetched %q, want the cover URL from the listing", images.urls[0])
	}
}

// An album listed before this store recorded covers has none, and asking Google for an empty
// URL would spend a request to be told so.
func TestAnAlbumWithNoCoverIsNotFetched(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})

	images := &stubImages{}
	server.images = images

	recorder := get(handler, "/album/holiday/cover", login(t, handler))
	if got := recorder.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("served as %q, want the placeholder", got)
	}
	if images.calls != 0 {
		t.Errorf("an album with no cover reached Google %d times", images.calls)
	}
}

// One item Google is unhappy about must not take the page down: the other 199 cells are
// still what the user came to pick from.
func TestAFailingThumbnailStillRendersACell(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 1)

	server.images = &stubImages{err: errors.New("google said no")}

	recorder := get(handler, "/thumb/AF1Qip0000", login(t, handler))
	if recorder.Code != http.StatusOK {
		t.Fatalf("a failed thumbnail returned %d, want a placeholder with 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("served as %q, want the placeholder", got)
	}
}

// The session is not always live — the daemon warms up every twelve hours, and a browser
// open in between must not turn a missing session into a broken page.
func TestAThumbnailWithoutASessionServesAPlaceholder(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 1)

	recorder := get(handler, "/thumb/AF1Qip0000", login(t, handler))
	if recorder.Code != http.StatusOK {
		t.Fatalf("a thumbnail with no session returned %d, want 200", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "image/svg+xml" {
		t.Errorf("the placeholder was served as %q", contentType)
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Errorf("the placeholder was cached as %q — the session may come back", cache)
	}
}

func TestAnUnknownThumbnailIsNotFound(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	if recorder := get(handler, "/thumb/nope", login(t, handler)); recorder.Code != http.StatusNotFound {
		t.Errorf("an unknown thumbnail returned %d, want 404", recorder.Code)
	}
}

// Nothing walks an album's items until a sync visits it, so an album freshly switched to
// "picked" has an empty grid. The explicit refresh is the way to re-walk one on demand.
func TestAnEmptyGridOffersToFetchTheAlbum(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 40})
	seedItems(t, server, "holiday", 1)
	cookie := login(t, handler)

	body := get(handler, "/album/holiday", cookie).Body.String()
	if !strings.Contains(body, "Refresh from Google") {
		t.Error("an album offers no way to re-fetch its contents")
	}

	recorder := postForm(handler, "/album/holiday/refresh", url.Values{"page": {"1"}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("refreshing returned %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); !strings.HasPrefix(location, "/album/holiday?page=1") {
		t.Errorf("redirected to %q, want back to the grid", location)
	}

	if calls := runsOf(server).calls(); len(calls) != 1 || !strings.HasPrefix(calls[0], "list:holiday") {
		t.Errorf("the runner was asked for %v, want a listing of holiday", calls)
	}
}

// Deciding whether to back an album up means looking inside it first, and picking items is
// impossible until something has listed them. So the first view lists the album itself rather
// than showing an empty page and a button.
func TestOpeningAnUnlistedAlbumFetchesItAutomatically(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 40})
	cookie := login(t, handler)

	body := get(handler, "/album/holiday", cookie).Body.String()

	calls := runsOf(server).calls()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "list:holiday") {
		t.Fatalf("opening an unlisted album asked for %v, want a listing of holiday", calls)
	}
	if strings.Contains(body, "Nothing listed for this album yet") {
		t.Error("the page still tells the user to refresh an album it is already fetching")
	}
	if !strings.Contains(body, "in progress") {
		t.Error("the page does not say a listing is running")
	}
}

// The page reloads itself while a run is in flight. If each reload started another listing,
// opening one album would queue a listing per poll for as long as the user left the tab open.
func TestARunningListingIsNotStartedTwice(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", ItemCount: 40})
	cookie := login(t, handler)

	get(handler, "/album/holiday", cookie)
	runsOf(server).activity = "Listing an album"
	get(handler, "/album/holiday", cookie)
	get(handler, "/album/holiday", cookie)

	if calls := runsOf(server).calls(); len(calls) != 1 {
		t.Errorf("polling an album being listed started %d listings, want 1: %v", len(calls), calls)
	}
}

func TestWhatDecidesAnAutomaticListing(t *testing.T) {
	listed := albumView{Total: 12, UpstreamTotal: 40}
	unlisted := albumView{Total: 0, UpstreamTotal: 40}

	cases := []struct {
		name  string
		view  albumView
		state auth.State
		want  bool
	}{
		{"an album nobody has listed", unlisted, auth.StateOK, true},
		{"an album already listed", listed, auth.StateOK, false},
		{"an album Google says is empty", albumView{}, auth.StateOK, false},
		{"an album being listed right now", albumView{UpstreamTotal: 40, Running: true}, auth.StateOK, false},
		{"a session that needs re-authentication", unlisted, auth.StateAuthRequired, false},
		{"a session nobody has warmed up yet", unlisted, auth.StateUnknown, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := shouldListOnFirstView(testCase.view, testCase.state); got != testCase.want {
				t.Errorf("listing on first view is %v, want %v", got, testCase.want)
			}
		})
	}
}

// An album in 'picked' mode with nothing picked downloads nothing while looking followed.
func TestAnAlbumWithNothingPickedIsFlagged(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 3)
	if err := server.store.SetAlbumSyncMode("holiday", store.SyncPicked); err != nil {
		t.Fatalf("switching to picked: %v", err)
	}
	cookie := login(t, handler)

	if body := get(handler, "/albums", cookie).Body.String(); !strings.Contains(body, "nothing picked") {
		t.Error("an album that backs up nothing is not flagged on the list")
	}

	postJSON(handler, "/album/holiday/picks",
		pickRequest{MediaKeys: []string{"AF1Qip0000"}, Selected: true}, cookie)

	if body := get(handler, "/albums", cookie).Body.String(); strings.Contains(body, "nothing picked") {
		t.Error("the flag survived a pick")
	}
}

// A tab left open all afternoon is not a statement about what has happened since. The review
// queue exists to make an approval durable, and the no-script save used to undo one silently:
// the item was on the page the server re-read at save time, and absent from the form the browser
// had rendered before anyone approved it.
func TestASavedPageClearsOnlyWhatItActuallyShowed(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	keys := seedItems(t, server, "holiday", 3)
	cookie := login(t, handler)

	if err := server.store.SetSelection(keys, true); err != nil {
		t.Fatalf("seeding the selection: %v", err)
	}

	// The page was drawn before the third item was approved elsewhere, so it says nothing about it.
	recorder := postForm(handler, "/album/holiday/picks",
		url.Values{"shown": keys[:2], "pick": keys[:1], "page": {"1"}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("saving the page returned %d, want 303", recorder.Code)
	}

	selected, err := server.store.SelectionIn("holiday")
	if err != nil {
		t.Fatalf("reading the selection back: %v", err)
	}
	if !selected[keys[0]] {
		t.Error("the item left ticked was cleared")
	}
	if selected[keys[1]] {
		t.Error("the item unticked on the page survived the save")
	}
	if !selected[keys[2]] {
		t.Error("a pick made after the page rendered was cleared by a form that never showed it")
	}
}

// The hidden fields are as much a claim as the ticks are, and a form is not evidence of what was
// on screen — so they are checked against the album before anything is cleared.
func TestASavedPageCannotClearPicksInAnotherAlbum(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server,
		store.Album{ID: "holiday", Title: "Holiday 2026"},
		store.Album{ID: "private", Title: "Not this one"})
	holiday := seedItems(t, server, "holiday", 1)
	cookie := login(t, handler)

	now := time.Now()
	elsewhere := store.MediaItem{MediaKey: "AF1QipELSEWHERE", Filename: "elsewhere.HEIC"}
	if err := server.store.UpsertItem(elsewhere, now); err != nil {
		t.Fatalf("seeding the other album's item: %v", err)
	}
	if err := server.store.LinkItemToAlbum("private", elsewhere.MediaKey, now); err != nil {
		t.Fatalf("linking the other album's item: %v", err)
	}
	if err := server.store.SetSelection([]string{elsewhere.MediaKey}, true); err != nil {
		t.Fatalf("seeding the other album's selection: %v", err)
	}

	postForm(handler, "/album/holiday/picks",
		url.Values{"shown": append(holiday, elsewhere.MediaKey), "page": {"1"}}, cookie)

	selected, err := server.store.SelectionIn("private")
	if err != nil {
		t.Fatalf("reading the other album's selection back: %v", err)
	}
	if !selected[elsewhere.MediaKey] {
		t.Error("a form named an album it was not looking at and cleared a pick in it")
	}
}

// The save handler clears by comparing the ticks against what the page says it drew, so a page
// that does not say leaves the handler unable to clear anything at all.
func TestAPickingPageReportsEveryCellItDrew(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	keys := seedItems(t, server, "holiday", 3)
	if err := server.store.SetAlbumSyncMode("holiday", store.SyncPicked); err != nil {
		t.Fatalf("switching to picked: %v", err)
	}
	cookie := login(t, handler)

	body := get(handler, "/album/holiday", cookie).Body.String()
	for _, key := range keys {
		if !strings.Contains(body, `<input type="hidden" name="shown" value="`+key+`">`) {
			t.Errorf("the page drew %s without reporting it; body was:\n%s", key, body)
		}
	}
}
