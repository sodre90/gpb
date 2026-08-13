package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"gpb/internal/store"
)

// star is the request the page's own form makes: the state wanted, not an instruction to flip.
func star(t *testing.T, handler http.Handler, path, albumID string, wanted bool, cookie *http.Cookie) {
	t.Helper()

	value := "no"
	if wanted {
		value = "yes"
	}
	recorder := postForm(handler, "/album/"+albumID+"/favourite"+path,
		url.Values{"favourite": {value}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("starring %s returned %d, want 303", albumID, recorder.Code)
	}
}

var albumLink = regexp.MustCompile(`href="/album/([a-z-]+)"`)

// albumsInOrder reads the list off the page in the order the page put them, which is the whole
// of what sorting means here — asserting on the sort function alone would pass while the table
// rendered something else.
func albumsInOrder(body string) []string {
	var albums []string
	for _, match := range albumLink.FindAllStringSubmatch(body, -1) {
		if !strings.Contains(strings.Join(albums, " "), match[1]) {
			albums = append(albums, match[1])
		}
	}
	return albums
}

// A star is the user's own answer to which of 181 albums matters. Sorting is how they look for
// something; it is not an instruction to bury the answer they already gave.
func TestAStarredAlbumLeadsTheListHoweverItIsSorted(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server,
		store.Album{ID: "alps", Title: "Alps", ItemCount: 900},
		store.Album{ID: "barcelona", Title: "Barcelona", ItemCount: 40},
		store.Album{ID: "zoo", Title: "Zoo", ItemCount: 2})
	cookie := login(t, handler)

	star(t, handler, "", "zoo", true, cookie)

	for _, arrangement := range []string{
		"", "?sort=title", "?sort=title&dir=desc", "?sort=items", "?sort=items&dir=desc",
		"?sort=created", "?sort=done&dir=desc", "?sort=mode",
	} {
		body := get(handler, "/albums"+arrangement, cookie).Body.String()
		if order := albumsInOrder(body); len(order) == 0 || order[0] != "zoo" {
			t.Errorf("/albums%s lists %v, want the starred album first", arrangement, order)
		}
	}
}

// The filter is in the URL rather than in the browser because sorting reloads this page: a
// filter the script held would be dropped by the first click on a heading, and every form on
// the page posts back through the same suffix.
func TestTheFavouritesFilterSurvivesEverythingOnThePage(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server,
		store.Album{ID: "alps", Title: "Alps"},
		store.Album{ID: "zoo", Title: "Zoo"},
		store.Album{ID: "bundle", Kind: store.AlbumBundle})
	cookie := login(t, handler)
	star(t, handler, "", "zoo", true, cookie)

	body := get(handler, "/albums?favourites=only", cookie).Body.String()
	if order := albumsInOrder(body); len(order) != 1 || order[0] != "zoo" {
		t.Errorf("the filtered list shows %v, want only the starred album", order)
	}
	if !strings.Contains(body, `aria-current="true"`) {
		t.Error("nothing on the page says the filter is on")
	}

	for _, heading := range (albumsQuery{FavouritesOnly: true}).headings() {
		if !strings.Contains(heading.Link, "favourites=only") {
			t.Errorf("sorting by %s links to %q, which drops the filter", heading.Label, heading.Link)
		}
	}

	recorder := postForm(handler, "/album/zoo/mode?favourites=only",
		url.Values{"mode": {"all"}}, cookie)
	if location := recorder.Header().Get("Location"); !strings.Contains(location, "favourites=only") {
		t.Errorf("after saving, the browser was sent to %q, which drops the filter", location)
	}

	// The bundles are a second table on the same page, filtered by the same control.
	opened := get(handler, "/albums?favourites=only&bundles=open", cookie).Body.String()
	if strings.Contains(opened, `href="/album/bundle"`) {
		t.Error("the shared-photos section ignores the favourites filter")
	}
}

// A filter that empties the table has to leave behind the way to turn itself off, or the only
// way out is the browser's own back button.
func TestFilteringToNothingStillOffersTheWayBack(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "alps", Title: "Alps"})
	cookie := login(t, handler)

	body := get(handler, "/albums?favourites=only", cookie).Body.String()
	if !strings.Contains(body, "albums-favourites") {
		t.Error("the filter header goes away with the rows, leaving no way to unfilter")
	}
	if !strings.Contains(body, "Nothing is a favourite yet") {
		t.Error("an empty filtered list does not say why it is empty")
	}
	if strings.Contains(body, "No albums known yet") {
		t.Error("a filtered list with nothing in it claims the account has no albums")
	}
}

// The page's script asks for the row back rather than reloading a list of 181. Unstarring while
// filtered to favourites leaves no row to send: the answer has to say so, or the script would
// put back the row the filter had just excluded.
func TestUnstarringWhileFilteredTakesTheRowOffThePage(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "zoo", Title: "Zoo"})
	cookie := login(t, handler)
	star(t, handler, "", "zoo", true, cookie)

	recorder := saveInPlace(handler, "/album/zoo/favourite?favourites=only",
		url.Values{"favourite": {"no"}}, cookie)

	var saved savedRow
	if err := json.Unmarshal(recorder.Body.Bytes(), &saved); err != nil {
		t.Fatalf("reading the saved row back: %v (body %q)", err, recorder.Body.String())
	}
	if !saved.Gone {
		t.Errorf("unstarring a row under the favourites filter answered with %q, want it marked gone",
			saved.Row)
	}
	if saved.Tally == "" {
		t.Error("the recount under the table did not come back with the change")
	}
}

// The whole page works without JavaScript, which is what the star being a form is for.
func TestTheStarIsAFormAnyBrowserCanPost(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "zoo", Title: "Zoo"})
	cookie := login(t, handler)

	body := get(handler, "/albums", cookie).Body.String()
	if !strings.Contains(body, `action="/album/zoo/favourite"`) {
		t.Fatal("the star is not a form, so a scriptless browser cannot set one")
	}
	if !strings.Contains(body, `aria-pressed="false"`) {
		t.Error("an unstarred album does not say so to a screen reader")
	}

	star(t, handler, "", "zoo", true, cookie)

	album, err := server.store.Album("zoo")
	if err != nil {
		t.Fatalf("reading the album back: %v", err)
	}
	if !album.Favourite {
		t.Fatal("posting the star did not record it")
	}
	if starred := get(handler, "/albums", cookie).Body.String(); !strings.Contains(starred, `aria-pressed="true"`) {
		t.Error("a starred album does not say so to a screen reader")
	}
}
