package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
)

// The grid element carries everything the script lays the whole grid out from: where to fetch
// cells, how many there are, which window the page itself drew, and the months in grid order.
func TestAGridPageCarriesItsTimeline(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 3)
	seedLooseItem(t, server, "AF1QipOld", time.Date(2019, 3, 1, 12, 0, 0, 0, time.UTC))

	body := get(handler, "/photos", cookie).Body.String()
	grid := body[strings.Index(body, `id="grid"`):]
	grid = grid[:strings.Index(grid, ">")]
	for _, want := range []string{`data-cells="/photos/cells"`, `data-total="4"`, `data-offset="0"`,
		fmt.Sprintf(`data-window="%d"`, pageSize)} {
		if !strings.Contains(grid, want) {
			t.Errorf("the photo grid lacks %s: %s", want, grid)
		}
	}
	months := regexp.MustCompile(`data-months="([^"]*)"`).FindStringSubmatch(grid)
	if months == nil {
		t.Fatalf("the photo grid carries no months: %s", grid)
	}
	decoded := strings.ReplaceAll(months[1], "&#34;", `"`)
	if !strings.HasSuffix(decoded, `{"month":"2019-03","count":1}]`) {
		t.Errorf("the photo page's months do not end with the oldest: %s", decoded)
	}

	body = get(handler, "/album/holiday", cookie).Body.String()
	grid = body[strings.Index(body, `id="grid"`):]
	grid = grid[:strings.Index(grid, ">")]
	if !strings.Contains(grid, `data-cells="/album/holiday/cells"`) || !strings.Contains(grid, `data-total="3"`) {
		t.Errorf("the album grid does not name its own cells and count: %s", grid)
	}
}

// A window of cells is the same markup the page draws, in the same order, and never more than
// a page of it — and it is a fragment, so a dead session answers 401 rather than a login page.
func TestAWindowOfCellsIsAPageOfTheGridInOrder(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 5)

	body := get(handler, "/photos/cells?offset=1&limit=2", cookie).Body.String()
	keys := regexp.MustCompile(`data-key="([^"]*)"`).FindAllStringSubmatch(body, -1)
	if len(keys) != 2 || keys[0][1] != "AF1Qip0003" || keys[1][1] != "AF1Qip0002" {
		t.Errorf("the second and third newest were asked for, and the window holds %v", keys)
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "<form") {
		t.Error("a window of cells came wrapped in a page")
	}

	body = get(handler, fmt.Sprintf("/photos/cells?offset=0&limit=%d", pageSize*3), cookie).Body.String()
	if got := strings.Count(body, `data-key="`); got != 5 {
		t.Errorf("a window over the end holds %d cells, want the 5 that exist", got)
	}

	recorder := get(handler, "/photos/cells?offset=0&limit=2", nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("a window without a session answered %d, want 401", recorder.Code)
	}
}

// An album's window carries the album's own checkbox, ticked the way the store has it, so a
// cell scrolled into view a minute after the page opened shows the pick made since.
func TestAnAlbumWindowCarriesThePicksAsTheyAreNow(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	if err := server.store.SetAlbumSyncMode("holiday", store.SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedItems(t, server, "holiday", 3)
	if err := server.store.SelectItem("AF1Qip0001", true); err != nil {
		t.Fatalf("picking an item: %v", err)
	}

	body := get(handler, "/album/holiday/cells?offset=0&limit=3", cookie).Body.String()
	cell := body[strings.Index(body, `data-key="AF1Qip0001"`):]
	cell = cell[:strings.Index(cell, "</figure>")]
	if !strings.Contains(cell, `name="pick"`) || !strings.Contains(cell, " checked") {
		t.Errorf("the picked item's cell is not a ticked checkbox: %s", cell)
	}
	other := body[strings.Index(body, `data-key="AF1Qip0000"`):]
	other = other[:strings.Index(other, "</figure>")]
	if strings.Contains(other, " checked") {
		t.Error("an unpicked item's cell arrived ticked")
	}
}

// A shift-click on the timeline names its two ends; the cells between may never have reached
// the browser, so the store fills them in.
func TestAPickRequestMayNameARangeByItsEnds(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	if err := server.store.SetAlbumSyncMode("holiday", store.SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedItems(t, server, "holiday", 6)

	recorder := postJSON(handler, "/album/holiday/picks",
		map[string]any{"range": map[string]string{"from": "AF1Qip0004", "to": "AF1Qip0001"}, "selected": true}, cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("a range pick answered %d: %s", recorder.Code, recorder.Body.String())
	}

	picked, err := server.store.SelectionIn("holiday")
	if err != nil {
		t.Fatalf("reading the selection: %v", err)
	}
	for _, key := range []string{"AF1Qip0001", "AF1Qip0002", "AF1Qip0003", "AF1Qip0004"} {
		if !picked[key] {
			t.Errorf("%s lies inside the range and is not picked", key)
		}
	}
	if picked["AF1Qip0000"] || picked["AF1Qip0005"] {
		t.Errorf("the range spilled past its ends: %v", picked)
	}
	if !strings.Contains(recorder.Body.String(), `"picked":4`) {
		t.Errorf("the answer does not count the range: %s", recorder.Body.String())
	}
}
