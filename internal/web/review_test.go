package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
)

// flagNewForReview puts an item into the state a new arrival in a 'picked' album is left in:
// still 'discovered', but wanting a decision before anything fetches it.
func flagNewForReview(t *testing.T, server *Server, mediaKey string) {
	t.Helper()
	if err := server.store.FlagForReview([]string{mediaKey}); err != nil {
		t.Fatalf("flagging %s for review: %v", mediaKey, err)
	}
}

func seedPickedItem(t *testing.T, server *Server, albumID, mediaKey string, at time.Time) {
	t.Helper()
	if err := server.store.UpsertItem(store.MediaItem{MediaKey: mediaKey, Filename: mediaKey + ".jpg"}, at); err != nil {
		t.Fatalf("seeding item %s: %v", mediaKey, err)
	}
	if err := server.store.LinkItemToAlbum(albumID, mediaKey, at); err != nil {
		t.Fatalf("linking %s to %s: %v", mediaKey, albumID, err)
	}
}

func seedMissingItem(t *testing.T, server *Server, albumID, mediaKey string, at time.Time) {
	t.Helper()
	seedPickedItem(t, server, albumID, mediaKey, at)
	if err := server.store.MarkMissingUpstream(mediaKey, at); err != nil {
		t.Fatalf("marking %s missing upstream: %v", mediaKey, err)
	}
}

// Answering both halves with one button would mean "approve" deciding to re-download something
// that no longer exists upstream, so the page must keep them visually and functionally apart.
func TestNewItemsAndGoneItemsAppearInSeparateHalvesWithDifferentButtons(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	now := time.Now()

	seedAlbums(t, server, store.Album{ID: "lake", Title: "Lake weekend", ItemCount: 1})
	if err := server.store.SetAlbumSyncMode("lake", store.SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedPickedItem(t, server, "lake", "new-1", now)
	flagNewForReview(t, server, "new-1")

	seedAlbums(t, server, store.Album{ID: "iceland", Title: "Iceland 2024", ItemCount: 1})
	if err := server.store.SetAlbumSyncMode("iceland", store.SyncAll); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedMissingItem(t, server, "iceland", "gone-1", now)

	body := get(handler, "/review", cookie).Body.String()
	newHalf, goneHalf, found := strings.Cut(body, "Gone from Google")
	if !found {
		t.Fatalf("the page has no Gone from Google section; body was:\n%s", body)
	}

	if !strings.Contains(newHalf, "Lake weekend") {
		t.Errorf("the new half is missing its album; body was:\n%s", newHalf)
	}
	if !strings.Contains(newHalf, `value="approve"`) || !strings.Contains(newHalf, `value="dismiss"`) {
		t.Errorf("the new half offers no approve/dismiss buttons; body was:\n%s", newHalf)
	}
	if strings.Contains(newHalf, `value="acknowledge"`) {
		t.Error("the new half offers an acknowledge button")
	}

	if !strings.Contains(goneHalf, "Iceland 2024") {
		t.Errorf("the gone half is missing its album; body was:\n%s", goneHalf)
	}
	if !strings.Contains(goneHalf, `value="acknowledge"`) {
		t.Errorf("the gone half offers no acknowledge button; body was:\n%s", goneHalf)
	}
	if strings.Contains(goneHalf, `value="approve"`) || strings.Contains(goneHalf, `value="dismiss"`) {
		t.Error("the gone half offers approve/dismiss buttons — approving it would queue a download that can never succeed")
	}
}

func TestApprovingAReviewedItemSelectsItAndClearsTheFlag(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	now := time.Now()

	seedAlbums(t, server, store.Album{ID: "lake", Title: "Lake weekend", ItemCount: 1})
	if err := server.store.SetAlbumSyncMode("lake", store.SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedPickedItem(t, server, "lake", "new-1", now)
	flagNewForReview(t, server, "new-1")

	recorder := postForm(handler, "/review/resolve",
		url.Values{"resolve": {"new-1"}, "decision": {"approve"}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("approving returned %d, want 303", recorder.Code)
	}

	selected, err := server.store.SelectionIn("lake")
	if err != nil {
		t.Fatalf("reading the selection back: %v", err)
	}
	if !selected["new-1"] {
		t.Error("approving did not select the item")
	}

	waiting, err := server.store.CountNeedingReview()
	if err != nil {
		t.Fatalf("counting the review queue: %v", err)
	}
	if waiting != 0 {
		t.Errorf("approving left %d items waiting for review, want 0", waiting)
	}
}

// Dismissing must settle the flag without ever selecting the item — an item left out on purpose
// must not be quietly queued for download anyway.
func TestDismissingAReviewedItemClearsTheFlagWithoutSelectingIt(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	now := time.Now()

	seedAlbums(t, server, store.Album{ID: "lake", Title: "Lake weekend", ItemCount: 1})
	if err := server.store.SetAlbumSyncMode("lake", store.SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedPickedItem(t, server, "lake", "new-1", now)
	flagNewForReview(t, server, "new-1")

	recorder := postForm(handler, "/review/resolve",
		url.Values{"resolve": {"new-1"}, "decision": {"dismiss"}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("dismissing returned %d, want 303", recorder.Code)
	}

	selected, err := server.store.SelectionIn("lake")
	if err != nil {
		t.Fatalf("reading the selection back: %v", err)
	}
	if selected["new-1"] {
		t.Error("dismissing selected the item anyway")
	}

	waiting, err := server.store.CountNeedingReview()
	if err != nil {
		t.Fatalf("counting the review queue: %v", err)
	}
	if waiting != 0 {
		t.Errorf("dismissing left %d items waiting for review, want 0", waiting)
	}
}

// The retention policy is that a photo Google has lost stays on disk regardless; acknowledging
// it must be the one verb that touches nothing but the flag.
func TestAcknowledgingAMissingItemLeavesItsStateAndFileAlone(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	now := time.Now()

	seedAlbums(t, server, store.Album{ID: "iceland", Title: "Iceland 2024", ItemCount: 1})
	if err := server.store.SetAlbumSyncMode("iceland", store.SyncAll); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedMissingItem(t, server, "iceland", "gone-1", now)

	recorder := postForm(handler, "/review/resolve",
		url.Values{"resolve": {"gone-1"}, "decision": {"acknowledge"}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("acknowledging returned %d, want 303", recorder.Code)
	}

	item, err := server.store.Item("gone-1")
	if err != nil {
		t.Fatalf("reading the item back: %v", err)
	}
	if item.State != store.StateMissingUpstream {
		t.Errorf("acknowledging changed the item's state to %q", item.State)
	}
	if item.NeedsReview {
		t.Error("acknowledging did not clear the review flag")
	}
}

// The ticked-by-default boxes are what a browser without script has instead of Select all, so
// the two have to arrive together: buttons that only script can honour must not be visible until
// it has run, and the boxes must be ticked whether or not it ever does.
func TestTheQueueArrivesTickedAndOffersSelectAllOnlyToScript(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	now := time.Now()

	seedAlbums(t, server, store.Album{ID: "lake", Title: "Lake weekend", ItemCount: 2})
	if err := server.store.SetAlbumSyncMode("lake", store.SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	for _, mediaKey := range []string{"new-1", "new-2"} {
		seedPickedItem(t, server, "lake", mediaKey, now)
		flagNewForReview(t, server, mediaKey)
	}

	body := get(handler, "/review", cookie).Body.String()
	if ticked := strings.Count(body, " checked"); ticked != 2 {
		t.Errorf("%d of 2 boxes arrived ticked; body was:\n%s", ticked, body)
	}
	if !strings.Contains(body, `<span class="grid-ticktools" hidden>`) {
		t.Errorf("the Select all buttons are not hidden from a scriptless browser; body was:\n%s", body)
	}
	if !strings.Contains(body, `/static/review.js`) {
		t.Errorf("nothing on the page reveals the Select all buttons; body was:\n%s", body)
	}
}

// The form carries whatever it carries. Approving a key the queue never offered would select a
// photo for download on the strength of a request rather than of anything the user saw.
func TestAResolveOnlyTouchesTheItemsTheQueueIsAskingAbout(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	now := time.Now()

	seedAlbums(t, server, store.Album{ID: "lake", Title: "Lake weekend", ItemCount: 2})
	if err := server.store.SetAlbumSyncMode("lake", store.SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedPickedItem(t, server, "lake", "new-1", now)
	flagNewForReview(t, server, "new-1")
	seedPickedItem(t, server, "lake", "never-asked-about", now)

	recorder := postForm(handler, "/review/resolve",
		url.Values{"resolve": {"new-1", "never-asked-about"}, "decision": {"approve"}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("approving returned %d, want 303", recorder.Code)
	}

	selected, err := server.store.SelectionIn("lake")
	if err != nil {
		t.Fatalf("reading the selection back: %v", err)
	}
	if !selected["new-1"] {
		t.Error("the item the queue asked about was not approved")
	}
	if selected["never-asked-about"] {
		t.Error("an item the queue never offered was selected for download by the request alone")
	}
}

// The cap is what stands between a queue page and a request that hands the daemon a hundred
// thousand keys to work through, so it has to refuse rather than settle the first few.
func TestAResolveNamingMoreKeysThanAPageHoldsIsRefused(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	now := time.Now()

	seedAlbums(t, server, store.Album{ID: "lake", Title: "Lake weekend", ItemCount: 1})
	if err := server.store.SetAlbumSyncMode("lake", store.SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedPickedItem(t, server, "lake", "new-1", now)
	flagNewForReview(t, server, "new-1")

	tooMany := make([]string, maxKeysPerRequest+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("key-%d", i)
	}
	tooMany[0] = "new-1"

	recorder := postForm(handler, "/review/resolve",
		url.Values{"resolve": tooMany, "decision": {"approve"}}, cookie)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized resolve returned %d, want 413", recorder.Code)
	}

	item, err := server.store.Item("new-1")
	if err != nil {
		t.Fatalf("reading the item back: %v", err)
	}
	if !item.NeedsReview {
		t.Error("a refused request settled an item anyway")
	}
}

// A queue that keeps asking about an album the user already declined never empties.
func TestItemsInADeclinedAlbumNeverReachTheQueue(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	now := time.Now()

	seedAlbums(t, server, store.Album{ID: "declined", Title: "Declined album", ItemCount: 1})
	seedMissingItem(t, server, "declined", "gone-1", now)

	body := get(handler, "/review", cookie).Body.String()
	if strings.Contains(body, "Declined album") {
		t.Errorf("an album nobody follows appeared in the review queue; body was:\n%s", body)
	}
}
