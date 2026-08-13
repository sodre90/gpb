package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"gpb/internal/auth"
	"gpb/internal/store"
	"gpb/internal/syncer"
)

func TestOnlyWhatMovedIsAnnounced(t *testing.T) {
	idle := liveSnapshot{activity: "", authState: auth.StateOK}

	for name, testCase := range map[string]struct {
		after liveSnapshot
		want  []string
	}{
		"nothing at all":     {idle, nil},
		"a run started":      {liveSnapshot{activity: "sync", authState: auth.StateOK}, []string{topicRun}},
		"the counts moved":   {liveSnapshot{progress: syncer.Progress{Listed: 1}, authState: auth.StateOK}, []string{topicProgress}},
		"google signed out":  {liveSnapshot{authState: auth.StateAuthRequired}, []string{topicSession}},
		"a warmup began":     {liveSnapshot{warming: true, authState: auth.StateOK}, []string{topicSession}},
		"the browser opened": {liveSnapshot{reauthRunning: true, authState: auth.StateOK}, []string{topicSession}},
		"a run and a count": {
			liveSnapshot{activity: "sync", progress: syncer.Progress{Downloaded: 3}, authState: auth.StateOK},
			[]string{topicRun, topicProgress},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := changedTopics(idle, testCase.after)
			if strings.Join(got, ",") != strings.Join(testCase.want, ",") {
				t.Errorf("changedTopics said %v, want %v", got, testCase.want)
			}
		})
	}
}

// A topic is a hint to look again, so a reader who was away wants it once however many times it
// was published. That is what stops a stalled socket growing a backlog nobody will ever read.
func TestAStalledReaderIsOwedEachTopicOnceRatherThanABacklog(t *testing.T) {
	client := newLiveClient(nil)

	for range 500 {
		client.queue(topicProgress)
		client.queue(topicRun)
	}

	owed := client.take()
	if len(owed) != 2 {
		t.Fatalf("a stalled reader is owed %d messages, want 2: %v", len(owed), owed)
	}
	if left := client.take(); len(left) != 0 {
		t.Errorf("taking the owed topics left %v behind", left)
	}
}

func TestTheSocketRefusesAnUpgradeFromAnotherOrigin(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	request := httptest.NewRequest(http.MethodGet, "/ws", nil)
	request.Header.Set("Origin", "http://somewhere.else")
	request.AddCookie(cookie)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Errorf("a cross-origin upgrade returned %d, want 403", recorder.Code)
	}
}

// A socket carries the same account's business as the pages do, so it answers to the same cookie.
func TestTheSocketRefusesAnUpgradeWithoutASession(t *testing.T) {
	server, _ := testServer(t)

	request := httptest.NewRequest(http.MethodGet, "/ws", nil)
	request.Header.Set("Origin", "http://example.test")

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code == http.StatusSwitchingProtocols {
		t.Error("an unauthenticated request was upgraded")
	}
}

func TestAPublishedTopicReachesAnOpenSocket(t *testing.T) {
	server, _ := testServer(t)
	front := httptest.NewServer(server.Handler())
	defer front.Close()

	cookie := login(t, server.Handler())
	address := strings.TrimPrefix(front.URL, "http://")

	dialer := ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{
		"Cookie": []string{cookie.String()},
		"Origin": []string{front.URL},
	})}

	conn, _, _, err := dialer.Dial(context.Background(), "ws://"+address+"/ws")
	if err != nil {
		t.Fatalf("dialling the socket: %v", err)
	}
	defer conn.Close()

	waitUntil(t, func() bool { return server.live.connected() == 1 })
	server.live.publish(topicRun)

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	message, err := wsutil.ReadServerText(conn)
	if err != nil {
		t.Fatalf("reading the notification: %v", err)
	}
	if got := string(message); got != `{"topic":"run"}` {
		t.Errorf("the socket carried %q, want the run topic alone", got)
	}
}

// Shutdown deliberately leaves hijacked connections alone, so the hub has to close them itself
// or a stopping daemon waits on sockets nobody is going to close.
func TestShuttingDownClosesOpenSockets(t *testing.T) {
	server, _ := testServer(t)
	front := httptest.NewServer(server.Handler())
	defer front.Close()

	cookie := login(t, server.Handler())
	address := strings.TrimPrefix(front.URL, "http://")

	dialer := ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{
		"Cookie": []string{cookie.String()},
		"Origin": []string{front.URL},
	})}

	conn, _, _, err := dialer.Dial(context.Background(), "ws://"+address+"/ws")
	if err != nil {
		t.Fatalf("dialling the socket: %v", err)
	}
	defer conn.Close()

	waitUntil(t, func() bool { return server.live.connected() == 1 })
	server.live.closeAll()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := wsutil.ReadServerText(conn); err == nil {
		t.Error("the socket was still readable after the hub closed it")
	}
	waitUntil(t, func() bool { return server.live.connected() == 0 })
}

func TestTheOverviewFragmentIsTheRegionAndNotAPage(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	recorder := get(handler, "/live/overview", cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /live/overview returned %d, want 200", recorder.Code)
	}

	body := recorder.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("the overview fragment sent a whole page; body was:\n%s", body)
	}
	if !strings.Contains(body, `id="live-overview"`) {
		t.Errorf("the overview fragment dropped the region the script replaces; body was:\n%s", body)
	}
}

// Every region has to name a topic and somewhere to fetch itself from, or the script silently
// leaves it alone for the rest of the page's life.
func TestEveryLiveRegionSaysWhereItComesFrom(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	for path, regions := range map[string][]string{
		"/":     {`id="live-overview" data-live="run progress session" data-live-src="/live/overview"`},
		"/runs": {`id="live-now" data-live="run progress" data-live-src="/live/runs/now"`, `id="live-history" data-live="run" data-live-src="/live/runs/history"`},
	} {
		body := get(handler, path, cookie).Body.String()
		for _, region := range regions {
			if !strings.Contains(body, region) {
				t.Errorf("%s is missing the region %q; body was:\n%s", path, region, body)
			}
		}
	}
}

// The nav is rendered per page, so the copy that replaces it has to be asked for in a way that
// says which page it is on, or every swap would move the current-page marker to the wrong link.
func TestTheNavFragmentKnowsWhichPageItIsOn(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	body := get(handler, "/albums", cookie).Body.String()
	if !strings.Contains(body, `data-live-src="/live/nav?on=%2Falbums"`) {
		t.Fatalf("the nav does not say which page it is on; body was:\n%s", body)
	}

	fragment := get(handler, "/live/nav?on=%2Falbums", cookie).Body.String()
	if !strings.Contains(fragment, `<a href="/albums" aria-current="page"`) {
		t.Errorf("the nav fragment marks a different page as current; markup was:\n%s", fragment)
	}
	if !strings.Contains(fragment, `data-live-src="/live/nav?on=%2Falbums"`) {
		t.Errorf("the nav fragment stops being a region after one swap; markup was:\n%s", fragment)
	}
}

func TestTheReviewPillFollowsTheQueue(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	quiet := get(handler, "/live/nav", cookie).Body.String()
	if strings.Contains(quiet, `class="topbar-count"`) {
		t.Errorf("an empty queue still showed a pill; markup was:\n%s", quiet)
	}

	quietSnapshot := server.liveSnapshot()

	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday", ItemCount: 1})
	if err := server.store.SetAlbumSyncMode("holiday", store.SyncPicked); err != nil {
		t.Fatalf("following the album: %v", err)
	}
	seedPickedItem(t, server, "holiday", "new-1", time.Now())
	flagNewForReview(t, server, "new-1")

	waiting := get(handler, "/live/nav", cookie).Body.String()
	if !strings.Contains(waiting, `class="topbar-count"`) {
		t.Errorf("the pill never appeared; markup was:\n%s", waiting)
	}
	if got := changedTopics(quietSnapshot, server.liveSnapshot()); !slices.Contains(got, topicReview) {
		t.Errorf("something joining the review queue announced %v, want the review topic among them", got)
	}
}

// The banner is the 'backups are silently broken' flag, so the empty container has to stay on the
// page: there is nowhere for the warning to appear otherwise.
func TestTheBannerRegionIsThereBeforeThereIsAnythingToSay(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	quiet := get(handler, "/live/banner", cookie).Body.String()
	if !strings.Contains(quiet, `id="live-session"`) {
		t.Errorf("the banner region vanishes when there is nothing to say; markup was:\n%s", quiet)
	}
	if strings.Contains(quiet, "session-banner") {
		t.Errorf("a healthy session was given a banner; markup was:\n%s", quiet)
	}
}

func TestTheLoginPageHasNoLiveRegions(t *testing.T) {
	server, _ := testServer(t)

	recorder := get(server.Handler(), "/login", nil)
	if strings.Contains(recorder.Body.String(), "data-live") {
		t.Errorf("the login page subscribes to a socket it cannot open; body was:\n%s", recorder.Body.String())
	}
}

func waitUntil(t *testing.T, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the socket to settle")
}

// The noVNC canvas must never be inside a region. Leaving the attributes off while the login
// browser is up is what makes that true by construction rather than by remembering.
func TestTheLoginBrowserIsNeverInsideALiveRegion(t *testing.T) {
	inFlight, err := renderBlock("reauth", "authactions", reauthView{Running: true, OwnedByMe: true, RemoteCanvas: true})
	if err != nil {
		t.Fatalf("rendering the actions with a login browser up: %v", err)
	}
	if strings.Contains(inFlight, "data-live") {
		t.Errorf("the login browser sits in a live region; markup was:\n%s", inFlight)
	}
	if !strings.Contains(inFlight, "<iframe") {
		t.Fatalf("this no longer renders the canvas, so it is testing nothing:\n%s", inFlight)
	}

	idle, err := renderBlock("reauth", "authactions", reauthView{})
	if err != nil {
		t.Fatalf("rendering the actions with no login browser: %v", err)
	}
	if !strings.Contains(idle, `data-live="session"`) {
		t.Errorf("the actions never follow the session at all; markup was:\n%s", idle)
	}
}

// The forms inside the albums region post back to the sort order the reader is looking at, so
// the fragment has to be asked for in that order too.
func TestTheAlbumsRegionKeepsTheSortOrderItWasRenderedIn(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday", ItemCount: 3})

	body := get(handler, "/albums?sort=items", cookie).Body.String()
	if !strings.Contains(body, `data-live-src="/live/albums/now?sort=items"`) {
		t.Errorf("the albums region drops the sort order; body was:\n%s", body)
	}
}

// A fragment that walked the album every time it was asked would turn one message from the
// server into an endless conversation with Google.
func TestAskingHowTheWalkIsGoingDoesNotStartAnother(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday", ItemCount: 3})

	before := len(runsOf(server).calls())
	if recorder := get(handler, "/live/album/holiday/listing", cookie); recorder.Code != http.StatusOK {
		t.Fatalf("GET the listing fragment returned %d, want 200", recorder.Code)
	}
	if after := len(runsOf(server).calls()); after != before {
		t.Errorf("asking about the walk started %d of them", after-before)
	}
}

// The whole point of the swap: the page that waited through a first-time walk gets the grid
// itself when the walk ends, and the region carrying it stops being one — from then on a message
// could only throw away a pick somebody was in the middle of making.
func TestTheGridArrivesWhenTheWalkEndsAndThenStopsMoving(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday", ItemCount: 3})
	seedItems(t, server, "holiday", 3)

	runsOf(server).activity = "listing an album"
	walking := get(handler, "/live/album/holiday/listing", cookie).Body.String()
	if !strings.Contains(walking, `data-live-src="/live/album/holiday/listing"`) {
		t.Errorf("a walk in flight stopped being followed; markup was:\n%s", walking)
	}
	if strings.Contains(walking, `id="grid"`) {
		t.Errorf("a walk still running handed over a grid it may add to; markup was:\n%s", walking)
	}

	runsOf(server).activity = ""
	arrived := get(handler, "/live/album/holiday/listing", cookie).Body.String()
	if !strings.Contains(arrived, `id="grid"`) {
		t.Errorf("the grid never arrived; markup was:\n%s", arrived)
	}
	if strings.Contains(arrived, "data-live") {
		t.Errorf("a grid with cells in it stayed live; markup was:\n%s", arrived)
	}
}

// A grid with cells in it holds picks a re-render would throw away, so it is never a region.
func TestAnAlbumWithPhotosInItIsNotLive(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday", ItemCount: 3})
	seedItems(t, server, "holiday", 3)

	body := get(handler, "/album/holiday", cookie).Body.String()
	if strings.Contains(body, `data-live-src="/live/album/`) {
		t.Errorf("an album grid declared itself live; body was:\n%s", body)
	}
}
