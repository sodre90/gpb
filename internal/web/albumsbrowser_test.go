package web

import (
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"gpb/internal/store"
)

// Starring saves without leaving the page, and the row it replaces has to stay part of the page
// afterwards — the search box beside it goes on searching the same table. Neither is a question
// the rendered HTML can answer: the server's reply is right in both cases, and the browser is
// where it either works or does not.
//
//	GPB_BROWSER=1 go test ./internal/web/ -run AlbumsPage
func TestTheAlbumsPageStarsARowAndGoesOnFindingIt(t *testing.T) {
	if os.Getenv("GPB_BROWSER") == "" {
		t.Skip("set GPB_BROWSER=1 to drive a real browser")
	}

	server, _ := testServer(t)
	// Alps sorts last of the three, so its arriving first afterwards is the star's doing.
	seedAlbums(t, server,
		store.Album{ID: "alps", Title: "Alps"},
		store.Album{ID: "barcelona", Title: "Barcelona"},
		store.Album{ID: "zermatt", Title: "Zermatt"})

	site := httptest.NewServer(server.Handler())
	defer site.Close()

	browser, done := openBrowser(t)
	defer done()
	logIn(t, browser, site.URL)

	const starOnAlps = `tr[data-title="Alps"] .albums-star button`
	if err := chromedp.Run(browser,
		chromedp.Navigate(site.URL+"/albums"),
		chromedp.WaitVisible("table.albums tbody tr"),
		// A page that reloaded loses this, which is the only difference between saving in place
		// and posting the form the way a scriptless browser would.
		chromedp.Evaluate(`window.neverReloaded = true`, nil),
		chromedp.Click(starOnAlps, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("starring an album: %v", err)
	}

	if err := chromedp.Run(browser, chromedp.Poll(
		`document.querySelector('`+starOnAlps+`').getAttribute("aria-pressed") === "true"`, nil,
		chromedp.WithPollingTimeout(10*time.Second))); err != nil {
		t.Fatalf("the star never came back pressed, so the row on the page still reads as unstarred: %v", err)
	}

	var kept bool
	if err := chromedp.Run(browser,
		chromedp.Evaluate(`window.neverReloaded === true`, &kept)); err != nil {
		t.Fatalf("asking whether the page reloaded: %v", err)
	}
	if !kept {
		t.Error("starring an album reloaded the whole page, which on a list of 181 throws the reader back to the top")
	}

	// Searching for an album other than the one just starred: the replaced row is the one a
	// filter working from a list remembered at load would leave on screen.
	if err := chromedp.Run(browser, chromedp.SendKeys("#filter", "barcelona", chromedp.ByQuery)); err != nil {
		t.Fatalf("typing in the search box: %v", err)
	}

	var showing []string
	if err := chromedp.Run(browser, chromedp.Evaluate(`Array.from(
		document.querySelectorAll("table.albums tbody tr")).filter((row) => !row.hidden)
		.map((row) => row.dataset.title)`, &showing)); err != nil {
		t.Fatalf("reading the filtered list: %v", err)
	}
	if len(showing) != 1 || showing[0] != "Barcelona" {
		t.Errorf("searching for \"barcelona\" leaves %v on the page, want only Barcelona", showing)
	}

	// The row does not jump to the top the moment it is starred, any more than changing a backup
	// mode re-sorts the table under the reader's hands. The pin is what the next load is in.
	var order []string
	if err := chromedp.Run(browser,
		chromedp.Navigate(site.URL+"/albums"),
		chromedp.WaitVisible("table.albums tbody tr"),
		chromedp.Evaluate(`Array.from(document.querySelectorAll("table.albums tbody tr"))
			.map((row) => row.dataset.title)`, &order),
	); err != nil {
		t.Fatalf("reloading the list: %v", err)
	}
	if len(order) == 0 || order[0] != "Alps" {
		t.Errorf("the reloaded list reads %v, want the starred album at the top of it", order)
	}
}
