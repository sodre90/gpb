package web

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
)

func day(year int, month time.Month, dayOfMonth int) time.Time {
	return time.Date(year, month, dayOfMonth, 0, 0, 0, 0, time.UTC)
}

func titlesInOrder(rows []albumRow) []string {
	titles := make([]string, 0, len(rows))
	for _, row := range rows {
		titles = append(titles, row.Title)
	}
	return titles
}

func sampleRows() []albumRow {
	return []albumRow{
		{Title: "Iceland", CreatedAt: day(2021, time.June, 1), ItemCount: 40, Done: 5, Mode: store.SyncNone},
		{Title: "alps", CreatedAt: day(2019, time.January, 2), ItemCount: 900, Done: 900, Mode: store.SyncAll},
		{Title: "Zoo", ItemCount: 3, Done: 0, Mode: store.SyncNone},
		{Title: "Beach", CreatedAt: day(2024, time.August, 9), ItemCount: 12, Done: 1, Mode: store.SyncPicked},
	}
}

func TestSortingByEachColumn(t *testing.T) {
	cases := []struct {
		order albumsQuery
		want  []string
	}{
		// Nobody has sorted this one, so it gets the default: the albums being backed up first,
		// then newest created, with the album whose date has never arrived last rather than
		// leading the list from year zero.
		{albumsQuery{}, []string{"Beach", "alps", "Iceland", "Zoo"}},
		{albumsQuery{Key: "title"}, []string{"alps", "Beach", "Iceland", "Zoo"}},
		{albumsQuery{Key: "title", Descending: true}, []string{"Zoo", "Iceland", "Beach", "alps"}},
		{albumsQuery{Key: "created"}, []string{"alps", "Iceland", "Beach", "Zoo"}},
		{albumsQuery{Key: "created", Descending: true}, []string{"Beach", "Iceland", "alps", "Zoo"}},
		{albumsQuery{Key: "items"}, []string{"Zoo", "Beach", "Iceland", "alps"}},
		{albumsQuery{Key: "done", Descending: true}, []string{"alps", "Iceland", "Beach", "Zoo"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.order.Key+dirLabel(testCase.order), func(t *testing.T) {
			rows := sampleRows()
			testCase.order.sort(rows)
			if got := titlesInOrder(rows); !equalStrings(got, testCase.want) {
				t.Errorf("sorted to %v, want %v", got, testCase.want)
			}
		})
	}
}

func dirLabel(order albumsQuery) string {
	if order.Descending {
		return "-desc"
	}
	return "-asc"
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

// Case-folding matters on a real library: without it every lowercase album sorts after every
// capitalised one, which reads as the list being unsorted.
func TestTitleSortIgnoresCase(t *testing.T) {
	rows := []albumRow{{Title: "Zebra"}, {Title: "apple"}, {Title: "Banana"}}
	albumsQuery{Key: "title"}.sort(rows)

	if got := titlesInOrder(rows); !equalStrings(got, []string{"apple", "Banana", "Zebra"}) {
		t.Errorf("sorted to %v, want case-insensitive order", got)
	}
}

// An album with no creation date is one waiting on a refresh, not one from the year zero.
// Sorting it to the top of a descending list would put the least informative rows first.
func TestAlbumsWithNoCreationDateSortLast(t *testing.T) {
	for _, descending := range []bool{false, true} {
		rows := sampleRows()
		albumsQuery{Key: "created", Descending: descending}.sort(rows)

		if last := rows[len(rows)-1].Title; last != "Zoo" {
			t.Errorf("descending=%v put %q last, want the album with no date", descending, last)
		}
	}
}

func TestAnUnknownSortColumnFallsBackToTheDefault(t *testing.T) {
	request := httptest.NewRequest("GET", "/?sort=id;DROP+TABLE+albums&dir=desc", nil)

	if order := queryFrom(request); order != (albumsQuery{}) {
		t.Errorf("an unknown column produced %+v, want the default order", order)
	}
}

func TestHeadingsFlipOnlyTheirOwnColumn(t *testing.T) {
	headings := albumsQuery{Key: "created", Descending: true}.headings()

	created, items := headings[1], headings[2]
	if created.Indicator != "▾" {
		t.Errorf("the sorted column shows %q, want a descending arrow", created.Indicator)
	}
	if !strings.Contains(created.Link, "sort=created") || strings.Contains(created.Link, "dir=desc") {
		t.Errorf("the sorted column links to %q, want it to flip back to ascending", created.Link)
	}
	if items.Indicator != "" {
		t.Errorf("an unsorted column shows %q, want no arrow", items.Indicator)
	}
	if !strings.Contains(items.Link, "dir=desc") {
		t.Errorf("Items links to %q, want the large end first", items.Link)
	}
}

// A user who sorted the list to find an album is still looking for it after changing its mode.
func TestChangingAModeKeepsTheSort(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday"})
	cookie := login(t, handler)

	recorder := postForm(handler, "/album/holiday/mode?sort=created&dir=desc",
		url.Values{"mode": {"all"}}, cookie)

	location := recorder.Header().Get("Location")
	if !strings.Contains(location, "sort=created") || !strings.Contains(location, "dir=desc") {
		t.Errorf("after saving, the browser was sent to %q, which drops the sort", location)
	}
}

// Google names a bundle of shared photos nowhere, so "Untitled album" reads as a library full
// of albums the user forgot to name rather than a limit of what Google gives us.
func TestBundlesOfSharedPhotosAreNotCalledUntitled(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server,
		store.Album{ID: "bundle", ItemCount: 4, Kind: store.AlbumBundle},
		store.Album{ID: "mine", ItemCount: 4, Kind: store.AlbumOwned})
	cookie := login(t, handler)

	body := get(handler, "/albums", cookie).Body.String()
	if !strings.Contains(body, "Shared photos") {
		t.Error("a bundle with no title is not identified as shared photos")
	}
	if !strings.Contains(body, "Untitled album") {
		t.Error("an owned album with no title should still read as untitled")
	}

	if grid := get(handler, "/album/bundle", cookie).Body.String(); !strings.Contains(grid,
		"Shared photos") {
		t.Error("the grid page still calls a bundle untitled")
	}
}

// Forty-nine rows all reading "Shared photos" is honest and useless. Whoever the photos came
// from is the only thing Google gives that separates one from the next.
func TestBundlesAreNamedAfterWhoeverSharedThem(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server,
		store.Album{ID: "mine", Kind: store.AlbumBundle,
			OwnerName: "Me Myself", OwnerIsAccount: true},
		store.Album{ID: "theirs", Kind: store.AlbumBundle, OwnerName: "Sam Sharer"},
		store.Album{ID: "nameless", Kind: store.AlbumBundle})

	body := get(handler, "/albums?bundles=open", login(t, handler)).Body.String()
	for _, want := range []string{"Photos you shared", "Shared photos from Sam Sharer", "Shared photos"} {
		if !strings.Contains(body, want) {
			t.Errorf("the album list never says %q", want)
		}
	}
}

// A bundle is not an album, and there are forty-nine of them. Listed among the real albums they
// are a wall of near-identical nameless rows between the ones the user came here for.
func TestBundlesAreListedApartFromAlbums(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server,
		store.Album{ID: "holiday", Title: "Holiday", Kind: store.AlbumOwned},
		store.Album{ID: "bundle", Kind: store.AlbumBundle})

	cookie := login(t, handler)
	body := get(handler, "/albums", cookie).Body.String()
	if !strings.Contains(body, "Shared photos (1)") {
		t.Error("bundles do not get a section of their own")
	}
	if !strings.Contains(body, "1 album, 0 under backup") {
		t.Error("the album count includes the bundle, which is not an album")
	}
	if strings.Contains(body, `href="/album/bundle"`) {
		t.Error("a collapsed section still renders its rows")
	}
	if opened := get(handler, "/albums?bundles=open", cookie).Body.String(); !strings.Contains(opened,
		`href="/album/bundle"`) {
		t.Error("the section cannot be opened")
	}
}

// Everything in this section is a link or a form POST, so the disclosure has to live in the
// URL: sorting the shared-photos table, or saving one of its rows, would otherwise close the
// section the user opened to reach it.
func TestOpeningTheSharedSectionSurvivesUsingIt(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "bundle", Kind: store.AlbumBundle})
	cookie := login(t, handler)

	for _, heading := range (albumsQuery{BundlesOpen: true}).headings() {
		if !strings.Contains(heading.Link, "bundles=open") {
			t.Errorf("sorting by %s links to %q, which closes the section", heading.Label, heading.Link)
		}
	}

	recorder := postForm(handler, "/album/bundle/mode?bundles=open",
		url.Values{"mode": {"all"}}, cookie)
	if location := recorder.Header().Get("Location"); !strings.Contains(location, "bundles=open") {
		t.Errorf("after saving, the browser was sent to %q, which closes the section", location)
	}
}

// The arrow turns down, the forty-nine rows appear, and until this was fixed the link under the
// arrow pointed at the page it was already on — so nothing on the page would put them away again.
func TestTheSharedSectionCanBeClosedAgain(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server,
		store.Album{ID: "holiday", Title: "Holiday", Kind: store.AlbumOwned},
		store.Album{ID: "bundle", Kind: store.AlbumBundle})
	cookie := login(t, handler)

	closing := (albumsQuery{BundlesOpen: true}).toggleBundlesLink()
	if strings.Contains(closing, "bundles=open") {
		t.Fatalf("the open section's own link is %q, which leaves it open", closing)
	}

	if body := get(handler, closing, cookie).Body.String(); strings.Contains(body, `href="/album/bundle"`) {
		t.Error("following the link from an open section still lists its rows")
	}
}

// Closing the section is not an excuse to lose the arrangement the reader set up around it.
func TestClosingTheSharedSectionKeepsTheRestOfThePage(t *testing.T) {
	open := albumsQuery{BundlesOpen: true, FavouritesOnly: true, Key: "title", Descending: true}

	closing := open.toggleBundlesLink()
	for _, want := range []string{"favourites=only", "sort=title", "dir=desc"} {
		if !strings.Contains(closing, want) {
			t.Errorf("closing the section links to %q, which drops %s", closing, want)
		}
	}
}

// A picture takes a throttled request to Google to arrive, so cells sit empty for seconds. A
// still grey square there reads as a broken photo rather than one on its way. The state has to be
// on the picture as it is served: images.js only ever takes it off, so a page that arrives without
// it shimmers nowhere and nothing says so.
func TestPicturesShimmerUntilTheyArrive(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday",
		CoverURL: "https://example.invalid/c"})
	seedItems(t, server, "holiday", 3)
	cookie := login(t, handler)

	for _, page := range []struct{ picture, path string }{
		{"an album cover", "/albums"},
		{"an item in the grid", "/album/holiday"},
	} {
		body := get(handler, page.path, cookie).Body.String()
		if !strings.Contains(body, `class="thumb loading"`) {
			t.Errorf("%s is served with no loading state, so it never shimmers", page.picture)
		}
		if !strings.Contains(body, "/static/images.js") {
			t.Errorf("nothing on %s ever stops the shimmer", page.path)
		}
	}

	if code := get(handler, "/static/images.js", cookie).Code; code != 200 {
		t.Errorf("the script the pages ask for returns %d", code)
	}
}

// A cover is the only thing that tells one nameless bundle from the next at a glance.
func TestRowsShowTheirCover(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server,
		store.Album{ID: "withcover", Title: "Holiday", CoverURL: "https://example.invalid/c"},
		store.Album{ID: "without", Title: "Zoo"})

	body := get(handler, "/albums", login(t, handler)).Body.String()
	if !strings.Contains(body, `src="/album/withcover/cover"`) {
		t.Error("an album with a cover does not show it")
	}
	if strings.Contains(body, `src="/album/without/cover"`) {
		t.Error("an album with no cover still asks for one")
	}
}

// An album shared with the user does have a name, and saying whose it is matters: it can
// change underneath them in a way their own albums cannot.
func TestSharedAlbumsSayWhoseTheyAre(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "theirs", Title: "kanari", Kind: store.AlbumShared})

	body := get(handler, "/albums", login(t, handler)).Body.String()
	if !strings.Contains(body, "kanari") {
		t.Error("a shared album's own title is not shown")
	}
	if !strings.Contains(body, "shared with you") {
		t.Error("a shared album is not marked as someone else's")
	}
}

func TestTheAlbumListShowsTheCreationDate(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday", CreatedAt: day(2021, time.June, 1)})

	body := get(handler, "/albums", login(t, handler)).Body.String()
	if !strings.Contains(body, "2021-06-01") {
		t.Error("the album list does not show the creation date")
	}
	if !strings.Contains(body, "sort=created") {
		t.Error("the Created heading is not clickable")
	}
}
