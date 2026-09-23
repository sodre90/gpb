package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
	"gpb/internal/syncer"
)

func seedAlbums(t *testing.T, server *Server, albums ...store.Album) {
	t.Helper()
	for _, album := range albums {
		if err := server.store.UpsertAlbum(album, time.Now()); err != nil {
			t.Fatalf("seeding album %s: %v", album.ID, err)
		}
	}
}

func TestAlbumsPageListsWhatCanBeBackedUp(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t,
		server,
		store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 1150},
		store.Album{ID: "family", Title: "Family", ItemCount: 312},
	)

	recorder := get(handler, "/albums", login(t, handler))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET / returned %d, want 200", recorder.Code)
	}

	body := recorder.Body.String()
	for _, want := range []string{"Holiday 2026", "1,150", "Family", "2 albums, 0 under backup"} {
		if !strings.Contains(body, want) {
			t.Errorf("the album list is missing %q; body was:\n%s", want, body)
		}
	}
}

// The summary has to tell "there is nothing to fetch" apart from "nobody has looked yet". Both
// show zero photos found, and reading the first for the second is how a backup that never ran
// gets mistaken for one that finished.
func TestTheSummarySaysWhatIsUnderBackup(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t,
		server,
		store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 1150},
		store.Album{ID: "family", Title: "Family", ItemCount: 312},
	)
	cookie := login(t, handler)

	body := get(handler, "/", cookie).Body.String()
	if !strings.Contains(body, "Nothing is being backed up yet") {
		t.Errorf("with no album followed the summary does not say so; body was:\n%s", body)
	}

	if err := server.store.SetAlbumSyncMode("holiday", store.SyncAll); err != nil {
		t.Fatalf("following an album: %v", err)
	}

	body = get(handler, "/", cookie).Body.String()
	for _, want := range []string{"Backup", "1 album", "Around 1,150 photos", "Nothing has been listed yet"} {
		if !strings.Contains(body, want) {
			t.Errorf("the summary is missing %q; body was:\n%s", want, body)
		}
	}
	if strings.Contains(body, "1462") {
		t.Error("the summary counts albums nobody asked to back up")
	}
}

// A library is larger than the disk it is usually pointed at, so what is left is as much a part
// of "is this backup healthy" as what has been taken. A run stops at the floor rather than
// filling the filesystem, and the only useful time to read that number is before it happens.
func TestTheSummarySaysHowMuchRoomIsLeft(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 2})
	if err := server.store.SetAlbumSyncMode("holiday", store.SyncAll); err != nil {
		t.Fatalf("following an album: %v", err)
	}
	seedItems(t, server, "holiday", 2)

	body := get(handler, "/", cookie).Body.String()
	if !strings.Contains(body, " free</p>") {
		t.Errorf("the summary does not say how much room is left; body was:\n%s", body)
	}
}

// A run that has not ended has no verdict, and printing the empty one gave "Last run 22:57:13:
// , 0 downloaded." — which reads as a finished backup that fetched nothing.
func TestARunInFlightIsNotReportedAsAFinishedOne(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	id, err := server.store.StartRun(time.Now())
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	runsOf(server).nowRunning("sync", syncer.Progress{RunID: id})

	body := get(handler, "/", cookie).Body.String()
	if !strings.Contains(body, "still going") {
		t.Errorf("a run in flight is not reported as running; body was:\n%s", body)
	}
	if strings.Contains(body, "downloaded.") {
		t.Error("a run in flight is reported with the counts of a finished one")
	}
}

func TestAFinishedRunReportsItsVerdict(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	id, err := server.store.StartRun(time.Now())
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	finished := store.SyncRun{ID: id, Outcome: store.OutcomeOK, Downloaded: 12}
	if err := server.store.FinishRun(finished, time.Now()); err != nil {
		t.Fatalf("finishing the run: %v", err)
	}

	body := get(handler, "/", cookie).Body.String()
	for _, want := range []string{"Last run", "ok", "12 downloaded"} {
		if !strings.Contains(body, want) {
			t.Errorf("the last run is missing %q; body was:\n%s", want, body)
		}
	}
}

// An unfinished run with nothing running is a run the daemon was killed under — which is not
// the same as one still working, and must not claim to be.
func TestAnInterruptedRunSaysSo(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	if _, err := server.store.StartRun(time.Now()); err != nil {
		t.Fatalf("starting a run: %v", err)
	}

	body := get(handler, "/", cookie).Body.String()
	if !strings.Contains(body, "never finished") {
		t.Errorf("an interrupted run is not reported as one; body was:\n%s", body)
	}
	if strings.Contains(body, "still going") {
		t.Error("an interrupted run claims to still be working")
	}
}

func TestEmptyAlbumListSaysHowToFillIt(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	body := get(handler, "/albums", login(t, handler)).Body.String()
	if !strings.Contains(body, "Refresh album list") {
		t.Errorf("a fresh install is given no way forward; body was:\n%s", body)
	}
}

func TestSettingAModeFollowsTheAlbum(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 3})
	cookie := login(t, handler)

	recorder := postForm(handler, "/album/holiday/mode", url.Values{"mode": {"all"}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("setting a mode returned %d, want 303", recorder.Code)
	}

	followed, err := server.store.FollowedAlbums()
	if err != nil {
		t.Fatalf("reading followed albums: %v", err)
	}
	if len(followed) != 1 || followed[0].ID != "holiday" {
		t.Fatalf("the store holds %v, want holiday followed", followed)
	}
}

// The selector saves without reloading the page, which works only if the answer carries the row
// to put in place of the one that changed — and the tally under the table, which moves with it.
func TestSavingAModeInPlaceAnswersWithTheRow(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 3})
	cookie := login(t, handler)

	recorder := saveInPlace(handler, "/album/holiday/mode", url.Values{"mode": {"all"}}, cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("saving in place returned %d, want 200", recorder.Code)
	}

	var saved struct {
		Row   string `json:"row"`
		Tally string `json:"tally"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&saved); err != nil {
		t.Fatalf("decoding the answer: %v", err)
	}

	for _, want := range []string{"<tr", "followed", "Holiday 2026", `value="all" selected`} {
		if !strings.Contains(saved.Row, want) {
			t.Errorf("the row sent back is missing %q; row was:\n%s", want, saved.Row)
		}
	}
	if !strings.Contains(saved.Tally, "1 album, 1 under backup") {
		t.Errorf("the tally sent back was %q", saved.Tally)
	}
}

// Without scripting the same form posts plainly, and must still be sent back to the list —
// otherwise the one screen the product is configured from stops working.
func TestSavingAModeWithoutScriptingStillRedirects(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	cookie := login(t, handler)

	recorder := postForm(handler, "/album/holiday/mode", url.Values{"mode": {"all"}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("a plain form post returned %d, want 303", recorder.Code)
	}
	if body := recorder.Body.String(); strings.Contains(body, `"row"`) {
		t.Errorf("a plain form post was answered with a fragment: %s", body)
	}
}

func saveInPlace(handler http.Handler, path string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.AddCookie(cookie)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// A sync mode is the user's instruction, and an unknown one must not be written: the store
// would then hold a value no query matches, and the album would silently stop syncing.
func TestAnUnknownModeIsRefused(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	cookie := login(t, handler)

	recorder := postForm(handler, "/album/holiday/mode", url.Values{"mode": {"everything"}}, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an invalid mode returned %d, want 400", recorder.Code)
	}

	albums, _ := server.store.Albums()
	if albums[0].SyncMode != store.SyncNone {
		t.Fatalf("the album mode became %q", albums[0].SyncMode)
	}
}

func TestSyncNowAndRefreshAskTheRunner(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	for path, want := range map[string]string{"/sync": "sync:web ui", "/albums/refresh": "refresh:web ui"} {
		if recorder := postForm(handler, path, nil, cookie); recorder.Code != http.StatusSeeOther {
			t.Fatalf("POST %s returned %d, want 303", path, recorder.Code)
		}
		if calls := runsOf(server).calls(); !slices.Contains(calls, want) {
			t.Errorf("POST %s produced %v, want it to contain %q", path, calls, want)
		}
	}
}

// Asking twice is what a person does when the first click showed nothing. It is a conflict,
// not a failure, and it must not read like the backup broke.
func TestAsecondRunIsRefusedPolitely(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	runsOf(server).err = syncer.ErrRunInProgress

	recorder := postForm(handler, "/sync", nil, login(t, handler))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("a second run returned %d, want 409", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "already running") {
		t.Errorf("the page does not say why nothing happened; body was:\n%s", body)
	}
}

// The counts move while a run is in flight, and the Now card is what moves them — swapped in
// place by live.js, which needs the card to be there to find. The page itself never reloads.
func TestTheOverviewFollowsARunWithoutReloading(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	runsOf(server).activity = "sync"
	body := get(handler, "/", cookie).Body.String()

	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("the overview reloads itself while a run is in flight")
	}
	if !strings.Contains(body, `id="nowcard"`) {
		t.Errorf("the overview has no live card for the script to keep fresh; body was:\n%s", body)
	}
}

// The album list is made of open selectors. A page that reloads itself every five seconds would
// throw away whichever choice was half-made when the timer went off, so this one says what is
// happening and points at the page where it can be watched instead.
func TestTheAlbumListNeverReloadsItselfUnderTheUser(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	runsOf(server).activity = "sync"
	runsOf(server).progress = syncer.Progress{Listed: 1204, Downloaded: 350}

	body := get(handler, "/albums", cookie).Body.String()
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("the album list reloads itself while a run is in flight")
	}
	for _, want := range []string{"sync in progress", "1,204 listed", "350 downloaded", `href="/runs"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the album list is missing %q while a run works; body was:\n%s", want, body)
		}
	}
}

// The followed album here is the one every other ordering would put last, so that passing means
// the rank was consulted rather than the title tiebreak happening to agree with it.
func TestFollowedAlbumsSortToTheTop(t *testing.T) {
	server, _ := testServer(t)
	seedAlbums(t,
		server,
		store.Album{ID: "aaa", Title: "Aaa", CreatedAt: time.Now()},
		store.Album{ID: "zzz", Title: "Zzz"},
	)
	if err := server.store.SetAlbumSyncMode("zzz", store.SyncAll); err != nil {
		t.Fatalf("following an album: %v", err)
	}

	view, err := server.albumsView(albumsQuery{})
	if err != nil {
		t.Fatalf("building the view: %v", err)
	}
	if view.Albums[0].ID != "zzz" {
		t.Errorf("the list leads with %q, want the followed album first", view.Albums[0].ID)
	}
	if view.Followed != 1 {
		t.Errorf("the view counts %d followed, want 1", view.Followed)
	}
}

// A heading that was clicked has to mean what it says. Pinning the followed albums through an
// explicit sort would leave a column that cannot be sorted by, which is worse than a default
// arrangement someone can click their way out of.
func TestChoosingAColumnSortsByThatColumnAlone(t *testing.T) {
	server, _ := testServer(t)
	seedAlbums(t,
		server,
		store.Album{ID: "aaa", Title: "Aaa"},
		store.Album{ID: "zzz", Title: "Zzz"},
	)
	if err := server.store.SetAlbumSyncMode("zzz", store.SyncAll); err != nil {
		t.Fatalf("following an album: %v", err)
	}

	view, err := server.albumsView(albumsQuery{Key: "title"})
	if err != nil {
		t.Fatalf("building the view: %v", err)
	}
	if view.Albums[0].ID != "aaa" {
		t.Errorf("sorted by title the list leads with %q, want the first title", view.Albums[0].ID)
	}
}

// The library is where every photo that is in no album lives, and until it can be followed
// those photos are backed up by nothing.
func TestTheLibraryCanBeFollowedWithADate(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	body := get(handler, "/albums", cookie).Body.String()
	if !strings.Contains(body, `name="since"`) {
		t.Fatalf("the album list offers no library date; body was:\n%s", body)
	}

	recorder := postForm(handler, "/library",
		url.Values{"mode": {"all"}, "since": {"2020-06-01"}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("following the library returned %d, want 303", recorder.Code)
	}

	library, err := server.store.Library()
	if err != nil {
		t.Fatalf("reading the library back: %v", err)
	}
	if library.SyncMode != store.SyncAll {
		t.Errorf("the library mode is %q, want all", library.SyncMode)
	}
	if want := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC); !library.Since.Equal(want) {
		t.Errorf("the library date is %s, want %s", library.Since, want)
	}
}

// The date is the difference between a few hundred photos and a hundred thousand. A date the
// server cannot read must not be silently dropped into "no bound at all".
func TestAnUnreadableLibraryDateChangesNothing(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	recorder := postForm(handler, "/library",
		url.Values{"mode": {"all"}, "since": {"last summer"}}, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unreadable date returned %d, want 400", recorder.Code)
	}

	library, _ := server.store.Library()
	if library.SyncMode != store.SyncNone {
		t.Errorf("the library was followed anyway, as %q", library.SyncMode)
	}
}

// An empty date is the deliberate "as far back as it goes", not a failure to fill the field.
func TestAnEmptyLibraryDateMeansTheWholeHistory(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	if recorder := postForm(handler, "/library",
		url.Values{"mode": {"all"}, "since": {""}}, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("an empty date returned %d, want 303", recorder.Code)
	}

	library, _ := server.store.Library()
	if library.SyncMode != store.SyncAll || !library.Since.IsZero() {
		t.Errorf("the library reads mode %q since %s", library.SyncMode, library.Since)
	}
}

// The library is not an album and must not be counted as one: it has no cover, no owner and no
// creation date, and a row for it in the table would sort among real albums by none of them.
func TestTheLibraryIsNotListedAmongTheAlbums(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 3})

	body := get(handler, "/albums", login(t, handler)).Body.String()
	if !strings.Contains(body, "1 album, 0 under backup") {
		t.Errorf("the library was counted as an album; body was:\n%s", body)
	}
}

func TestTheSummarySaysWhatTheBackupTakesAndHowMuchIsVideo(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 2})
	if err := server.store.SetAlbumSyncMode("holiday", store.SyncAll); err != nil {
		t.Fatalf("following an album: %v", err)
	}
	keys := seedItems(t, server, "holiday", 2)
	now := time.Now()
	if err := server.store.UpsertItem(store.MediaItem{MediaKey: keys[1], Filename: "clip.mp4", IsVideo: true}, now); err != nil {
		t.Fatalf("marking the video: %v", err)
	}
	for key, size := range map[string]int64{keys[0]: 2_000_000, keys[1]: 3_000_000_000} {
		item := store.MediaItem{MediaKey: key, Filename: key, LocalPath: "/pool/" + key, SizeBytes: size, SHA256: key}
		if err := server.store.MarkDownloaded(item, now); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}

	body := get(handler, "/", cookie).Body.String()
	if !strings.Contains(body, `<span class="stat-value">3.0 GB</span> <span class="stat-label">on disk</span>`) {
		t.Errorf("the summary does not say what the backup takes; body was:\n%s", body)
	}
	if !strings.Contains(body, "photos 2.0 MB · videos 3.0 GB") {
		t.Errorf("the summary does not split photos from videos; body was:\n%s", body)
	}
}

// Until the linking pass reaches them, the second copies of a photo held under two keys are files
// of their own. The store counts the photo once, so the figure has to add them back or it says
// the pool takes less than the disk shows — 854.6 GB against 1,432 GB on the box, 2026-09-23.
func TestTheSummaryCountsSecondCopiesNotYetLinked(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026", ItemCount: 1})
	if err := server.store.SetAlbumSyncMode("holiday", store.SyncAll); err != nil {
		t.Fatalf("following an album: %v", err)
	}
	keys := seedItems(t, server, "holiday", 1)
	item := store.MediaItem{MediaKey: keys[0], Filename: keys[0], LocalPath: "/pool/" + keys[0], SizeBytes: 3_000_000_000, SHA256: keys[0]}
	if err := server.store.MarkDownloaded(item, time.Now()); err != nil {
		t.Fatalf("marking downloaded: %v", err)
	}
	runsOf(server).copies = syncer.Linking{Photos: 1, Separate: 1, Held: 1_000_000_000}

	body := get(handler, "/", cookie).Body.String()
	if !strings.Contains(body, `<span class="stat-value">4.0 GB</span> <span class="stat-label">on disk</span>`) {
		t.Errorf("the summary leaves out the copies not yet linked; body was:\n%s", body)
	}
	if !strings.Contains(body, "1.0 GB in second copies not yet linked") {
		t.Errorf("the summary does not say why; body was:\n%s", body)
	}

	runsOf(server).copies = syncer.Linking{}
	if body := get(handler, "/", cookie).Body.String(); strings.Contains(body, "not yet linked") {
		t.Error("the summary still mentions unlinked copies once there are none")
	}
}
