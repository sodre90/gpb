package web

import (
	"cmp"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"gpb/internal/store"
)

// albumsQuery is the state of the album list: how it is arranged, and whether the shared-photos
// section is open. The zero value means nobody has chosen, which is newest album first — the
// order the account itself grew in, and the one that puts this summer at the top where a person
// looking for a recent trip expects it.
//
// The disclosure lives in the URL rather than in the browser because everything on this page is
// a link or a form POST: sorting the shared-photos table, or saving one of its rows, would
// otherwise close the section the user opened to reach it.
type albumsQuery struct {
	Key         string
	Descending  bool
	BundlesOpen bool
	// FavouritesOnly narrows both tables to the starred albums. It lives in the URL rather than
	// in the page's script, unlike the title search beside it, because sorting reloads the page:
	// a filter held in the browser would be dropped by the first click on a heading.
	FavouritesOnly bool
}

// sortableColumn pairs a heading with the comparison behind it. Comparisons are written
// ascending; descending reverses them, so each one is stated once.
type sortableColumn struct {
	Key     string
	Label   string
	Class   string
	Numeric bool
	// DescendingFirst is for the columns where the interesting end is the large one. Nobody
	// clicking "Created" wants the oldest album first.
	DescendingFirst bool
	Compare         func(a, b albumRow) int
	// Missing marks rows the column has no value for. They sort last in both directions, which
	// is why this is separate from Compare: reversing for descending would otherwise turn
	// "unknown last" into "unknown first" and lead the list with its least useful rows.
	Missing func(albumRow) bool
}

var sortableColumns = []sortableColumn{
	{Key: "title", Label: "Album", Compare: byTitle},
	{Key: "created", Label: "Created", Class: "albums-created", DescendingFirst: true, Compare: byCreated, Missing: hasNoCreationDate},
	{Key: "items", Label: "Items", Class: "albums-items", Numeric: true, DescendingFirst: true, Compare: byItemCount},
	{Key: "done", Label: "Backed up", DescendingFirst: true, Compare: byBackedUp},
	{Key: "mode", Label: "Back up", Compare: byMode},
}

// byTitle folds case so that "iceland" and "Iceland" sort together rather than in two runs
// separated by every capitalised album in the account.
// byTitle sorts on the name the row actually shows. Google titles none of the 49 bundles, so
// comparing the stored title would order that whole section by opaque id — the one arrangement
// that carries no meaning at all.
func byTitle(a, b albumRow) int {
	return cmp.Or(
		strings.Compare(strings.ToLower(a.DisplayTitle()), strings.ToLower(b.DisplayTitle())),
		strings.Compare(a.ID, b.ID))
}

func byCreated(a, b albumRow) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), byTitle(a, b))
}

// hasNoCreationDate is true for an album waiting on a refresh, not one from the year zero:
// nothing knows an album's creation date until a listing has carried it.
func hasNoCreationDate(row albumRow) bool {
	return row.CreatedAt.IsZero()
}

func byItemCount(a, b albumRow) int {
	return cmp.Or(cmp.Compare(a.ItemCount, b.ItemCount), byTitle(a, b))
}

func byBackedUp(a, b albumRow) int {
	return cmp.Or(cmp.Compare(a.Done, b.Done), byTitle(a, b))
}

func byMode(a, b albumRow) int {
	return cmp.Or(cmp.Compare(backupRank(a.Mode), backupRank(b.Mode)), byTitle(a, b))
}

func backupRank(mode store.SyncMode) int {
	if mode == store.SyncNone {
		return 1
	}
	return 0
}

// queryFrom reads the list's state out of a request. An unknown column is not an error worth a
// page about — a hand-edited or stale link simply gets the default order, and the disclosure is
// read either way so a bad sort key cannot also close the section.
func queryFrom(r *http.Request) albumsQuery {
	query := albumsQuery{
		BundlesOpen:    r.URL.Query().Get("bundles") == openBundles,
		FavouritesOnly: r.URL.Query().Get("favourites") == onlyFavourites,
	}
	if key := r.URL.Query().Get("sort"); columnFor(key) != nil {
		query.Key = key
		query.Descending = r.URL.Query().Get("dir") == "desc"
	}
	return query
}

const (
	openBundles    = "open"
	onlyFavourites = "only"
)

func columnFor(key string) *sortableColumn {
	for index := range sortableColumns {
		if sortableColumns[index].Key == key {
			return &sortableColumns[index]
		}
	}
	return nil
}

const defaultSortKey = "created"

// effective is the order actually in force. A list nobody has sorted is still in some order, and
// resolving the unstated default in one place is what keeps the arrow, the flip a heading offers
// and the rows themselves from each having their own opinion about what that order is.
func (o albumsQuery) effective() (*sortableColumn, bool) {
	if column := columnFor(o.Key); column != nil {
		return column, o.Descending
	}
	return columnFor(defaultSortKey), true
}

// unchosen reports that nobody has picked a column, which is the only arrangement that keeps the
// followed albums on top. Somebody who clicked a heading asked for that column and nothing else;
// somebody who clicked nothing is looking at 181 albums of which a handful are being backed up,
// and burying those under a hundred they have never followed is what the list is for avoiding.
func (o albumsQuery) unchosen() bool {
	return columnFor(o.Key) == nil
}

func (o albumsQuery) sort(rows []albumRow) {
	column, descending := o.effective()
	pinFollowed := o.unchosen()

	slices.SortStableFunc(rows, func(a, b albumRow) int {
		// A star is the user's own answer to which of 181 albums matters, and it outranks every
		// column they could sort by. It sits outside the reversal below on purpose: reversing it
		// would bury the favourites at the bottom on the second click of a heading, which is not
		// what anyone means by sorting a table.
		if ranked := favouriteRank(a) - favouriteRank(b); ranked != 0 {
			return ranked
		}
		if pinFollowed {
			if ranked := backupRank(a.Mode) - backupRank(b.Mode); ranked != 0 {
				return ranked
			}
		}
		if ranked := missingRank(column, a) - missingRank(column, b); ranked != 0 {
			return ranked
		}
		if descending {
			return column.Compare(b, a)
		}
		return column.Compare(a, b)
	})
}

func favouriteRank(row albumRow) int {
	if row.Favourite {
		return 0
	}
	return 1
}

func missingRank(column *sortableColumn, row albumRow) int {
	if column.Missing != nil && column.Missing(row) {
		return 1
	}
	return 0
}

// headings renders every column as a link that flips its own direction and leaves the others
// alone, which is what a user clicking a table header expects to happen.
func (o albumsQuery) headings() []albumColumn {
	headings := make([]albumColumn, 0, len(sortableColumns))
	for _, column := range sortableColumns {
		headings = append(headings, albumColumn{
			Label:     column.Label,
			Link:      o.linkFor(column),
			Indicator: o.indicatorFor(column),
			Numeric:   column.Numeric,
			Class:     column.Class,
			Sort:      o.sortStateFor(column),
		})
	}
	return headings
}

func (o albumsQuery) linkFor(column sortableColumn) string {
	descending := column.DescendingFirst
	if inForce, was := o.effective(); inForce.Key == column.Key {
		descending = !was
	}

	query := url.Values{"sort": {column.Key}}
	if descending {
		query.Set("dir", "desc")
	}
	o.keepWhatIsShown(query)
	return albumsPath + "?" + query.Encode()
}

// toggleBundlesLink is the shared-photos section's own disclosure. It is a link rather than a
// <details> element so that the state survives the sort links and mode forms inside the
// section, which all reload the page and would otherwise close it.
func (o albumsQuery) toggleBundlesLink() string {
	flipped := o
	flipped.BundlesOpen = !o.BundlesOpen
	return flipped.link()
}

// sortStateFor is the arrow said out loud, for a reader who cannot see it.
func (o albumsQuery) sortStateFor(column sortableColumn) string {
	inForce, descending := o.effective()
	switch {
	case inForce.Key != column.Key:
		return ""
	case descending:
		return "descending"
	default:
		return "ascending"
	}
}

func (o albumsQuery) indicatorFor(column sortableColumn) string {
	inForce, descending := o.effective()
	switch {
	case inForce.Key != column.Key:
		return ""
	case descending:
		return "▾"
	default:
		return "▴"
	}
}

// keep carries the current state across a redirect, so changing one album's mode does not
// silently throw away the arrangement the user set up to find it — nor close the section they
// opened to reach it.
func (o albumsQuery) keep(query url.Values) url.Values {
	o.keepWhatIsShown(query)
	if o.Key == "" {
		return query
	}
	query.Set("sort", o.Key)
	if o.Descending {
		query.Set("dir", "desc")
	}
	return query
}

// keepWhatIsShown carries the two decisions about which rows are on the page at all, as opposed
// to the order they are in. Both have to survive a sort link, or clicking a heading would put
// back the hundred rows the reader had just narrowed away.
func (o albumsQuery) keepWhatIsShown(query url.Values) {
	if o.BundlesOpen {
		query.Set("bundles", openBundles)
	}
	if o.FavouritesOnly {
		query.Set("favourites", onlyFavourites)
	}
}

// toggleFavouritesLink is the filter header's own control: this same page with the filter the
// other way about, which is the only thing that changes.
func (o albumsQuery) toggleFavouritesLink() string {
	flipped := o
	flipped.FavouritesOnly = !o.FavouritesOnly
	return flipped.link()
}

func (o albumsQuery) link() string {
	query := o.keep(url.Values{})
	if len(query) == 0 {
		return albumsPath
	}
	return albumsPath + "?" + query.Encode()
}

// suffix is the same order as something a form can post back through, since a form action is
// a URL and not a query the handler can be handed directly.
func (o albumsQuery) suffix() string {
	query := o.keep(url.Values{})
	if len(query) == 0 {
		return ""
	}
	return "?" + query.Encode()
}
