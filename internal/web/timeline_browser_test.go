package web

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"gpb/internal/store"
)

// Whether a drag along the rail lands on the month it names, and whether a grid of hundreds
// holds only the cells near the viewport, are questions no rendered HTML can answer: the rows
// are placed by script from measurements only a browser can make. So this drives a real one,
// and only when asked.
//
//	GPB_BROWSER=1 go test ./internal/web/ -run Timeline
func TestTheTimelineRailLandsOnTheMonthDraggedTo(t *testing.T) {
	if os.Getenv("GPB_BROWSER") == "" {
		t.Skip("set GPB_BROWSER=1 to drive a real browser")
	}

	server, _ := testServer(t)
	seedAlbums(t, server, store.Album{ID: "years", Title: "Years of it"})
	if err := server.store.SetAlbumSyncMode("years", store.SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedAWeekApart(t, server, "years", 450)

	site := httptest.NewServer(server.Handler())
	defer site.Close()
	browser, done := openBrowser(t)
	defer done()
	logIn(t, browser, site.URL)
	failOnScriptErrors(t, browser)

	var floating, label string
	var cellsInDocument int
	if err := chromedp.Run(browser,
		chromedp.Navigate(site.URL+"/album/years"),
		chromedp.WaitVisible(".grid.timeline .grid-cell[data-key]"),
		chromedp.Evaluate(dragTheRailTo(0.5), nil),
		chromedp.Sleep(time.Second),
		chromedp.Text(".timeline-now", &floating, chromedp.ByQuery),
		chromedp.Text(".rail-label", &label, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelectorAll('#grid .grid-cell[data-key]').length`, &cellsInDocument),
	); err != nil {
		t.Fatalf("dragging the rail: %v", err)
	}

	// Halfway along the rail is halfway through the items — the 225th of 450, captured 224 weeks
	// ago by the seed's own arithmetic. The floating month and the rail's label have to agree.
	middle := time.Now().Add(-(450 - 1 - 225) * 7 * 24 * time.Hour)
	want := middle.Format("January 2006")
	if floating != want || label != want {
		t.Errorf("halfway along the rail the page says %q and the rail says %q, want %q", floating, label, want)
	}
	if cellsInDocument == 0 || cellsInDocument >= 450 {
		t.Errorf("%d of 450 cells are in the document; the grid should hold only those near the viewport", cellsInDocument)
	}
}

// A shift-click on the timeline may span cells the browser never fetched, and the viewer may
// be stepped to a photo that was never on screen. Both have to come out right.
func TestTheTimelinePicksRangesAndStepsAcrossWindowsItNeverShowed(t *testing.T) {
	if os.Getenv("GPB_BROWSER") == "" {
		t.Skip("set GPB_BROWSER=1 to drive a real browser")
	}

	server, _ := testServer(t)
	seedAlbums(t, server, store.Album{ID: "years", Title: "Years of it"})
	if err := server.store.SetAlbumSyncMode("years", store.SyncPicked); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}
	seedAWeekApart(t, server, "years", 450)

	site := httptest.NewServer(server.Handler())
	defer site.Close()
	browser, done := openBrowser(t)
	defer done()
	logIn(t, browser, site.URL)
	failOnScriptErrors(t, browser)

	var lastShown, counted, caption, counter string
	if err := chromedp.Run(browser,
		chromedp.Navigate(site.URL+"/album/years"),
		chromedp.WaitVisible(".grid.timeline .grid-cell[data-key]"),
		chromedp.Click(`.grid-cell[data-key="years-0000"] label`, chromedp.ByQuery),
		chromedp.Evaluate(`window.scrollTo(0, document.body.scrollHeight * 0.7)`, nil),
		chromedp.Sleep(time.Second),
		chromedp.Evaluate(`(() => {
			const cell = Array.from(document.querySelectorAll('#grid .grid-cell[data-key]')).at(-1);
			cell.querySelector('label').dispatchEvent(new MouseEvent('click', {bubbles: true, shiftKey: true}));
			return cell.dataset.key; })()`, &lastShown),
		chromedp.Sleep(time.Second),
		chromedp.Text("#pickcount", &counted, chromedp.ByQuery),
		chromedp.Evaluate(`window.scrollTo(0, 0)`, nil),
		chromedp.Sleep(time.Second),
		chromedp.Click(`.grid-cell[data-key="years-0000"] [data-open]`, chromedp.ByQuery),
		chromedp.WaitVisible("dialog.viewer figcaption"),
		chromedp.Evaluate(`document.querySelector('dialog.viewer').dispatchEvent(new KeyboardEvent('keydown', {key: 'ArrowLeft'}))`, nil),
		chromedp.Sleep(time.Second),
		chromedp.Text("dialog.viewer figcaption", &caption, chromedp.ByQuery),
		chromedp.Text("dialog.viewer .viewer-counter", &counter, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("driving the album: %v", err)
	}

	var last int
	if _, err := fmt.Sscanf(lastShown, "years-%d", &last); err != nil || last < 200 {
		t.Fatalf("the shift-click landed on %q, which is not past the first window", lastShown)
	}
	picked, err := server.store.SelectionIn("years")
	if err != nil {
		t.Fatalf("reading the selection: %v", err)
	}
	if len(picked) != last+1 || counted != fmt.Sprint(last+1) {
		t.Errorf("a range to item %d picked %d items in the store and %s on the page", last, len(picked), counted)
	}
	if counter != "450 of 450" || caption == "" {
		t.Errorf("stepping back from the first photo showed %q as %q, want the last of 450", caption, counter)
	}
}

// A thumbnail is one throttled request to Google, and the browser goes on loading an image
// after its cell has left the document. A jump along the rail must let go of the pictures it
// left behind, or five jumps put five screens nobody is looking at ahead of the one they are.
func TestAJumpAlongTheRailLetsGoOfTheThumbnailsItLeftBehind(t *testing.T) {
	if os.Getenv("GPB_BROWSER") == "" {
		t.Skip("set GPB_BROWSER=1 to drive a real browser")
	}

	server, _ := testServer(t)
	google := &unhurriedThumbnails{inFlight: map[string]int{}}
	server.images = google
	seedAlbums(t, server, store.Album{ID: "years", Title: "Years of it"})
	seedAWeekApart(t, server, "years", 450)

	site := httptest.NewServer(server.Handler())
	defer site.Close()
	browser, done := openBrowser(t)
	defer done()
	logIn(t, browser, site.URL)
	failOnScriptErrors(t, browser)

	var inDocument []string
	if err := chromedp.Run(browser,
		chromedp.Navigate(site.URL+"/album/years"),
		chromedp.WaitVisible(".grid.timeline .grid-cell[data-key]"),
		chromedp.Sleep(time.Second),
		chromedp.Evaluate(dragTheRailTo(0.5), nil),
		chromedp.Sleep(time.Second),
		chromedp.Evaluate(dragTheRailTo(0.9), nil),
		chromedp.Sleep(2*time.Second),
		chromedp.Evaluate(`Array.from(document.querySelectorAll('#grid img.thumb[src]')).map((image) => image.src.split('/').pop())`, &inDocument),
	); err != nil {
		t.Fatalf("jumping along the rail: %v", err)
	}

	shown := map[string]bool{}
	for _, key := range inDocument {
		shown[key] = true
	}
	inFlight, asked := google.snapshot()
	if len(inFlight) == 0 || asked == 0 {
		t.Fatalf("no thumbnail was ever asked for: %d in flight of %d asked", len(inFlight), asked)
	}
	for _, key := range inFlight {
		if !shown[key] {
			t.Errorf("Google is still being asked for %s, which is no longer on the page", key)
		}
	}
	t.Logf("%d thumbnails asked for over two jumps, %d still in flight, %d on the page", asked, len(inFlight), len(inDocument))
}

// unhurriedThumbnails stands in for Google and never answers, so what remains in flight is
// exactly what the browser has not let go of. Its keys are the tail of the seeded URLs.
type unhurriedThumbnails struct {
	mu       sync.Mutex
	inFlight map[string]int
	asked    int
}

func (u *unhurriedThumbnails) Thumbnail(ctx context.Context, baseURL string, w io.Writer) (int64, error) {
	key := baseURL[strings.LastIndex(baseURL, "/")+1:]
	u.mu.Lock()
	u.inFlight[key]++
	u.asked++
	u.mu.Unlock()
	<-ctx.Done()
	u.mu.Lock()
	u.inFlight[key]--
	if u.inFlight[key] == 0 {
		delete(u.inFlight, key)
	}
	u.mu.Unlock()
	return 0, ctx.Err()
}

func (u *unhurriedThumbnails) snapshot() (inFlight []string, asked int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for key := range u.inFlight {
		inFlight = append(inFlight, key)
	}
	return inFlight, u.asked
}

// seedAWeekApart fills an album with items captured a week apart, ending now, so the months
// on its timeline are known from the count alone.
func seedAWeekApart(t *testing.T, server *Server, albumID string, count int) {
	t.Helper()
	now := time.Now()
	for index := range count {
		key := fmt.Sprintf("%s-%04d", albumID, index)
		item := store.MediaItem{MediaKey: key, Filename: key + ".jpg",
			ThumbnailURL: "https://photos.fife.usercontent.google.com/pw/" + key,
			CapturedAt:   now.Add(-time.Duration(count-1-index) * 7 * 24 * time.Hour)}
		if err := server.store.UpsertItem(item, now); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
		if err := server.store.LinkItemToAlbum(albumID, key, now); err != nil {
			t.Fatalf("linking %s: %v", key, err)
		}
	}
}

func failOnScriptErrors(t *testing.T, browser context.Context) {
	t.Helper()
	chromedp.ListenTarget(browser, func(event any) {
		if thrown, ok := event.(*runtime.EventExceptionThrown); ok {
			t.Errorf("the page threw: %s", thrown.ExceptionDetails.Error())
		}
	})
}

// dragTheRailTo is a pointer pressed on the rail at a fraction of its height and let go.
func dragTheRailTo(fraction float64) string {
	return fmt.Sprintf(`(() => {
		const track = document.querySelector('.rail-track').getBoundingClientRect();
		const rail = document.querySelector('.rail');
		const at = { clientX: track.left + 4, clientY: track.top + track.height * %g, bubbles: true, pointerId: 1 };
		rail.dispatchEvent(new PointerEvent('pointerdown', at));
		rail.dispatchEvent(new PointerEvent('pointermove', at));
		rail.dispatchEvent(new PointerEvent('pointerup', at));
		return true; })()`, fraction)
}
