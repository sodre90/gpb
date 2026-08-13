package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
	"gpb/internal/syncer"
)

func TestAFinishedRunAppearsInHistoryWithItsOutcomeAndCounts(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	id, err := server.store.StartRun(time.Now())
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	finished := store.SyncRun{
		ID: id, Outcome: store.OutcomeOK, Listed: 4211, Downloaded: 122, Bytes: 47_000_000,
	}
	if err := server.store.FinishRun(finished, time.Now()); err != nil {
		t.Fatalf("finishing the run: %v", err)
	}

	body := get(handler, "/runs", cookie).Body.String()
	for _, want := range []string{"4,211", "122", "ok"} {
		if !strings.Contains(body, want) {
			t.Errorf("the run history is missing %q; body was:\n%s", want, body)
		}
	}
}

// An interrupted run and a working one are the same row in the database — a started run with no
// finished_at — so the page tells them apart by which run the daemon is writing to. Anything else
// that happens to be busy is no evidence at all: an album listing opens no run row, and reading
// "still going" from it relabelled a run that died yesterday, Now card and all.
func TestAnUnfinishedRunIsInterruptedUnlessItIsTheOneRunning(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	id, err := server.store.StartRun(time.Now())
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}

	body := get(handler, "/runs", cookie).Body.String()
	if !strings.Contains(body, "interrupted") {
		t.Errorf("an unfinished run with nothing running is not reported as interrupted; body was:\n%s", body)
	}
	if strings.Contains(body, "still going") {
		t.Error("an unfinished run with nothing running claims to still be going")
	}

	runsOf(server).nowRunning("album listing", syncer.Progress{})
	body = get(handler, "/runs", cookie).Body.String()
	if strings.Contains(body, "still going") {
		t.Errorf("an album listing brought a dead run back to life; body was:\n%s", body)
	}
	if !strings.Contains(body, "interrupted") {
		t.Error("the interrupted run stopped saying so while something unrelated ran")
	}

	runsOf(server).nowRunning("sync", syncer.Progress{RunID: id})
	body = get(handler, "/runs", cookie).Body.String()
	if !strings.Contains(body, "still going") {
		t.Errorf("the run the daemon is writing to is not reported as still going; body was:\n%s", body)
	}
	if strings.Contains(body, "interrupted") {
		t.Error("a run that is still going is also reported as interrupted")
	}
}

// The Now card's start time is the run's own, so it may only be shown for a run that is really
// running. Borrowed from the last row it read "backing up, started 19 hours ago" over an album
// listing that had been going for four seconds.
func TestTheNowCardBorrowsNoStartTimeFromADeadRun(t *testing.T) {
	server, _ := testServer(t)

	yesterday := time.Now().Add(-19 * time.Hour)
	id, err := server.store.StartRun(yesterday)
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}

	runsOf(server).nowRunning("album listing", syncer.Progress{})
	if card := server.nowCard(); card == nil {
		t.Fatal("an album listing in flight reports nothing at all")
	} else if card.StartedAt != "" {
		t.Errorf("an album listing borrowed the start time %q from a run that had already died", card.StartedAt)
	}

	runsOf(server).nowRunning("sync", syncer.Progress{RunID: id})
	if card := server.nowCard(); card.StartedAt != humanTime(yesterday) {
		t.Errorf("the run in flight reports a start time of %q, want its own", card.StartedAt)
	}
}

func TestWithNoRunsAtAllTheEmptyStateOffersABackUpNowButton(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	body := get(handler, "/runs", login(t, handler)).Body.String()
	if !strings.Contains(body, "No runs yet") {
		t.Errorf("a fresh install is not told no runs have happened; body was:\n%s", body)
	}
	if !strings.Contains(body, `action="/sync"`) {
		t.Errorf("the empty state offers no way to back up now; body was:\n%s", body)
	}
}

func TestTheNowCardAppearsOnlyWhileSomethingIsRunningWithItsLiveCounts(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	body := get(handler, "/runs", cookie).Body.String()
	if strings.Contains(body, `id="nowcard"`) {
		t.Error("the Now card appears with nothing running")
	}

	runsOf(server).activity = "sync"
	runsOf(server).progress = syncer.Progress{Listed: 1204, Downloaded: 350, Failed: 6}

	body = get(handler, "/runs", cookie).Body.String()
	if !strings.Contains(body, `id="nowcard"`) {
		t.Errorf("the Now card does not appear while a run is in flight; body was:\n%s", body)
	}
	for _, want := range []string{"1,204", "350", "6"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Now card is missing its live count %q; body was:\n%s", want, body)
		}
	}
}

// The container has to survive a run ending, because it is the only place the next run has to
// appear on a page that will never reload to discover one started.
func TestTheNowFragmentKeepsItsContainerWhenNothingIsRunning(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	recorder := get(handler, "/live/runs/now", cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /live/runs/now with nothing running returned %d, want 200", recorder.Code)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `id="live-now"`) {
		t.Errorf("the fragment dropped the region the script replaces; body was:\n%s", body)
	}
	if strings.Contains(body, "in progress") {
		t.Errorf("the fragment claims a run is in flight when none is; body was:\n%s", body)
	}
}

// The card used to carry a bar that only said "something is happening", which on a backup that
// takes days is the one thing the person watching already knows.
func TestTheNowCardNamesWhatIsBeingDownloadedUnderTheAlbumItCameFrom(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 4)

	runsOf(server).activity = "sync"
	runsOf(server).progress = syncer.Progress{
		Listed: 1204, Downloaded: 350, Owed: 1000,
		Items: []syncer.InFlight{
			{MediaKey: "key-a", Filename: "IMG_2044.MOV", AlbumID: "holiday", Written: 78, Total: 100},
			{MediaKey: "key-b", Filename: "IMG_2045.HEIC", AlbumID: "holiday", Written: 9, Total: 100},
		},
	}

	body := get(handler, "/runs", cookie).Body.String()
	for _, want := range []string{"Holiday 2026", "IMG_2044.MOV", "78%", "IMG_2045.HEIC", "9%", "350 / 1,000"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Now card is missing %q; body was:\n%s", want, body)
		}
	}
	if rows := strings.Count(body, `class="progress-row progress-album"`); rows != 1 {
		t.Errorf("two files from one album drew %d album rows, want 1; body was:\n%s", rows, body)
	}
}

// A percentage needs a length, and the length of an item is not known until the content host
// declares it. Until then the card says what has arrived rather than inventing a fraction.
func TestAFileOfUnknownLengthShowsWhatHasArrivedRatherThanAPercentage(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})

	runsOf(server).activity = "sync"
	runsOf(server).progress = syncer.Progress{
		Downloaded: 1, Owed: 2,
		Items: []syncer.InFlight{{MediaKey: "key-a", Filename: "IMG_2044.MOV", AlbumID: "holiday", Written: 2_500_000}},
	}

	body := get(handler, "/runs", cookie).Body.String()
	if !strings.Contains(body, "2.5 MB") {
		t.Errorf("a file with no declared length does not say what has arrived; body was:\n%s", body)
	}
	if strings.Contains(body, `class="tally">0%`) {
		t.Errorf("a file with no declared length was given a percentage; body was:\n%s", body)
	}
}

// A run that is only listing has nothing to put a denominator on, and a bar with no number
// behind it is the animation this replaced.
func TestARunWithNothingOwedYetShowsNoRunBar(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	runsOf(server).activity = "refresh"
	runsOf(server).progress = syncer.Progress{Listed: 300}

	body := get(handler, "/runs", cookie).Body.String()
	if strings.Contains(body, `class="meter"`) {
		t.Errorf("a listing run drew a progress bar it cannot fill; body was:\n%s", body)
	}
}

// A fragment is a fragment: answering with a whole document would nest one page inside another
// at the point the script swaps it in.
func TestTheNowFragmentCarriesTheCardAloneWhileARunIsInFlight(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	runsOf(server).activity = "sync"
	runsOf(server).progress = syncer.Progress{Listed: 40, Downloaded: 12}

	recorder := get(handler, "/live/runs/now", cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /live/runs/now with a run in flight returned %d, want 200", recorder.Code)
	}

	body := recorder.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("the fragment sent a whole page rather than the card; body was:\n%s", body)
	}
	for _, want := range []string{"Now", "40", "12"} {
		if !strings.Contains(body, want) {
			t.Errorf("the live fragment is missing %q; body was:\n%s", want, body)
		}
	}
}
