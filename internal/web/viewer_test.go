package web

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"

	"gpb/internal/store"
)

// Whether a photograph fits the window is a question no rendered HTML can answer. The viewer
// once sized a 4000-pixel photograph by its width alone and hung the rest of it off the bottom
// of the screen, with every template assertion in this package still passing: the markup was
// right and the layout was wrong. So this one drives a real browser, and only when asked.
//
//	GPB_BROWSER=1 go test ./internal/web/ -run Viewer
func TestTheViewerFitsAPhotographToTheWindowAndZoomsIn(t *testing.T) {
	if os.Getenv("GPB_BROWSER") == "" {
		t.Skip("set GPB_BROWSER=1 to drive a real browser")
	}

	server, _ := testServer(t)
	server.images = paintedThumbnails{}
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday", SyncMode: store.SyncAll})
	seedPhotographsLargerThanAnyWindow(t, server, "holiday", 2)

	site := httptest.NewServer(server.Handler())
	defer site.Close()

	browser, done := openBrowser(t)
	defer done()
	logIn(t, browser, site.URL)

	if err := chromedp.Run(browser,
		chromedp.Navigate(site.URL+"/album/holiday"),
		chromedp.WaitVisible(".grid-cell"),
		chromedp.Click(".grid-cell [data-open]", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("opening the viewer: %v", err)
	}

	opened := measureTheViewer(t, browser)
	if opened.Natural.H <= opened.Stage.H {
		t.Fatalf("the photograph is %.0f tall and the window %.0f: nothing here could hang off the "+
			"bottom of the screen however the stylesheet behaved", opened.Natural.H, opened.Stage.H)
	}
	if opened.Media.W > opened.Stage.W+1 || opened.Media.H > opened.Stage.H+1 {
		t.Errorf("the photograph opens at %.0f×%.0f in a window of %.0f×%.0f, so most of it cannot be seen",
			opened.Media.W, opened.Media.H, opened.Stage.W, opened.Stage.H)
	}
	if opened.Figure.H > opened.Dialog.H+1 {
		t.Errorf("the photograph and its caption stand %.0f tall in a screen of %.0f, so the caption "+
			"is below the bottom of it", opened.Figure.H, opened.Dialog.H)
	}

	if err := chromedp.Run(browser, chromedp.Click("dialog.viewer img", chromedp.ByQuery)); err != nil {
		t.Fatalf("clicking the photograph: %v", err)
	}
	closer := measureTheViewer(t, browser)
	if closer.Media.W <= opened.Media.W {
		t.Errorf("a click left the photograph %.0f wide, the same as before: it did not zoom in",
			closer.Media.W)
	}

	if err := chromedp.Run(browser, chromedp.Click("dialog.viewer img", chromedp.ByQuery)); err != nil {
		t.Fatalf("clicking the photograph a second time: %v", err)
	}
	if back := measureTheViewer(t, browser); back.Media.W > back.Stage.W+1 {
		t.Errorf("a second click left the photograph %.0f wide in a window of %.0f: a click zooms "+
			"in but nothing takes it back out", back.Media.W, back.Stage.W)
	}

	// The wheel zooms about the pointer, which is to say whatever was under the pointer is still
	// under it afterwards. Zoom about the middle instead and the detail you leaned in to look at
	// is the first thing to leave the screen.
	fitted := measureTheViewer(t, browser)
	pointerX, pointerY := fitted.Media.X+fitted.Media.W/4, fitted.Media.Y+fitted.Media.H/4
	if err := chromedp.Run(browser, turnTheWheel(pointerX, pointerY, -240)); err != nil {
		t.Fatalf("turning the wheel over the photograph: %v", err)
	}
	wheeled := measureTheViewer(t, browser)
	if wheeled.Media.W <= fitted.Media.W {
		t.Errorf("the wheel left the photograph %.0f wide, the same as before: it did not zoom in",
			wheeled.Media.W)
	}
	across := (pointerX - wheeled.Media.X) / wheeled.Media.W
	down := (pointerY - wheeled.Media.Y) / wheeled.Media.H
	if math.Abs(across-0.25) > 0.01 || math.Abs(down-0.25) > 0.01 {
		t.Errorf("the pointer was a quarter of the way into the photograph and after zooming it is "+
			"%.2f across and %.2f down: the photograph grew out of its middle, not out of the pointer",
			across, down)
	}

	// Dragging is how you look at the far corner of a photograph too big for the window. The
	// gesture ends in what the browser calls a click, and a viewer that took that click would
	// zoom straight back out of whatever the drag was leaning in to see.
	fromX, fromY := wheeled.Stage.X+wheeled.Stage.W/2, wheeled.Stage.Y+wheeled.Stage.H/2
	if err := chromedp.Run(browser, dragThePhotograph(fromX, fromY, fromX-200, fromY)); err != nil {
		t.Fatalf("dragging the photograph: %v", err)
	}
	panned := measureTheViewer(t, browser)
	if math.Abs(panned.Media.X-(wheeled.Media.X-200)) > 2 {
		t.Errorf("a drag of 200 to the left moved the photograph from %.0f to %.0f, not to %.0f",
			wheeled.Media.X, panned.Media.X, wheeled.Media.X-200)
	}
	if math.Abs(panned.Media.W-wheeled.Media.W) > 1 {
		t.Errorf("the photograph was %.0f wide and the drag left it %.0f: the end of the drag was "+
			"taken for a click", wheeled.Media.W, panned.Media.W)
	}

	if err := chromedp.Run(browser, chromedp.Click(".viewer-on", chromedp.ByQuery)); err != nil {
		t.Fatalf("stepping to the next photograph: %v", err)
	}
	stepped := measureTheViewer(t, browser)
	if stepped.Media.W > stepped.Stage.W+1 || stepped.Media.H > stepped.Stage.H+1 {
		t.Errorf("the next photograph opens at %.0f×%.0f in a window of %.0f×%.0f: it is still "+
			"zoomed in from the one before", stepped.Media.W, stepped.Media.H, stepped.Stage.W, stepped.Stage.H)
	}

	// A photograph that fits while the viewer still believes it is zoomed in looks perfectly
	// right and answers the next click by zooming out of a photograph that was never zoomed in.
	if err := chromedp.Run(browser, chromedp.Click("dialog.viewer img", chromedp.ByQuery)); err != nil {
		t.Fatalf("clicking the next photograph: %v", err)
	}
	again := measureTheViewer(t, browser)
	if again.Media.W <= stepped.Media.W {
		t.Errorf("a click on the photograph stepped to left it %.0f wide: the zoom is still holding "+
			"the one before it", again.Media.W)
	}
}

// chromedp has no wheel of its own, and a wheel event made in the page would be testing a script
// dispatching events to itself rather than a browser.
func turnTheWheel(x, y, delta float64) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		return input.DispatchMouseEvent(input.MouseWheel, x, y).WithDeltaX(0).WithDeltaY(delta).Do(ctx)
	})
}

func dragThePhotograph(fromX, fromY, toX, toY float64) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for _, event := range []*input.DispatchMouseEventParams{
			input.DispatchMouseEvent(input.MousePressed, fromX, fromY).WithButton(input.Left).WithButtons(1).WithClickCount(1),
			input.DispatchMouseEvent(input.MouseMoved, (fromX+toX)/2, (fromY+toY)/2).WithButton(input.Left).WithButtons(1),
			input.DispatchMouseEvent(input.MouseMoved, toX, toY).WithButton(input.Left).WithButtons(1),
			input.DispatchMouseEvent(input.MouseReleased, toX, toY).WithButton(input.Left).WithClickCount(1),
		} {
			if err := event.Do(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

// The boxes are the ones the browser draws, so the media's is what the zoom transform made of it
// rather than what the layout asked for.
type viewerBoxes struct {
	Dialog  box `json:"dialog"`
	Figure  box `json:"figure"`
	Stage   box `json:"stage"`
	Media   box `json:"media"`
	Natural box `json:"natural"`
}

type box struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

func measureTheViewer(t *testing.T, browser context.Context) viewerBoxes {
	t.Helper()

	var boxes viewerBoxes
	if err := chromedp.Run(browser,
		chromedp.Poll(`(() => {
			const image = document.querySelector("dialog.viewer img");
			return !!image && image.complete && image.naturalWidth > 0;
		})()`, nil, chromedp.WithPollingTimeout(30*time.Second)),
		chromedp.Evaluate(`(() => {
			const drawn = (selector) => {
				const found = document.querySelector(selector).getBoundingClientRect();
				return {x: found.left, y: found.top, w: found.width, h: found.height};
			};
			const image = document.querySelector("dialog.viewer img");
			return {
				dialog: drawn("dialog.viewer"),
				figure: drawn("dialog.viewer figure"),
				stage: drawn(".viewer-stage"),
				media: drawn("dialog.viewer img"),
				natural: {w: image.naturalWidth, h: image.naturalHeight},
			};
		})()`, &boxes),
	); err != nil {
		t.Fatalf("measuring the viewer: %v", err)
	}
	return boxes
}

// Photographs bigger than any window they will be shown in, which is the only kind that can hang
// off the bottom of one, painted here so the test carries no photograph of anybody's.
func seedPhotographsLargerThanAnyWindow(t *testing.T, server *Server, albumID string, count int) {
	t.Helper()

	now := time.Now()
	pool := server.cfg.PoolDir()
	if err := os.MkdirAll(pool, 0o755); err != nil {
		t.Fatalf("making the pool: %v", err)
	}

	for index := range count {
		key := fmt.Sprintf("AF1QipLarge%d", index)
		path := filepath.Join(pool, key+".jpg")
		file, err := os.Create(path)
		if err != nil {
			t.Fatalf("making %s: %v", path, err)
		}
		canvas := image.NewRGBA(image.Rect(0, 0, 3000, 2000))
		for y := range 2000 {
			for x := range 3000 {
				canvas.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), uint8(80 + index*60), 0xff})
			}
		}
		if err := jpeg.Encode(file, canvas, nil); err != nil {
			t.Fatalf("painting %s: %v", path, err)
		}
		file.Close()

		item := store.MediaItem{
			MediaKey:     key,
			Filename:     fmt.Sprintf("LARGE_%d.JPG", index),
			CapturedAt:   now.Add(time.Duration(index) * time.Minute),
			ThumbnailURL: "https://photos.fife.usercontent.google.com/pw/" + key,
			LocalPath:    path,
			SizeBytes:    1,
		}
		if err := server.store.UpsertItem(item, now); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
		if err := server.store.LinkItemToAlbum(albumID, key, now); err != nil {
			t.Fatalf("linking %s: %v", key, err)
		}
		if err := server.store.MarkDownloaded(item, now); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}
}
