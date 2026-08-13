package gphotos

import (
	"context"
	"time"
)

const albumsRPC = "F2A0H"

// Positions inside one album entry, measured against the Phase 0 captures. They are the
// part of this protocol most likely to drift, so they are named rather than inlined.
const (
	albumKindIndex      = 0
	albumTitleIndex     = 1
	albumCoverIndex     = 2
	albumItemCountIndex = 3
	albumCreatedAtIndex = 4
	albumIDIndex        = 6
	albumOwnerIndex     = 10
)

// Positions inside one person record. Slot 10 is the list of people an album or bundle belongs
// to, first the one who made it: measured 2026-08-10 it decoded on all 181 entries, named four
// different people across albums the account can see, and on the 49 bundles it named the
// account holder 48 times and one other person once — which matches who actually shared them.
const (
	personIDIndex   = 1
	personNameIndex = 11
)

// bundleKind is the value slot 0 carries for an entry that is not an album at all but a set
// of photos someone shared. Measured 2026-08-10 across 181 entries: the split is exact — the
// 132 albums carry 1 and the 49 bundles carry 4.
//
// A bundle has no name anywhere. Its own page is headed "Shared photos", its contents
// response carries no title, and it appears nowhere in the albums UI. Two thirds of them hold
// a single item. Telling them apart from albums is what stops the list claiming the user
// forgot to name 49 albums.
const bundleKind = 4

const (
	albumListEntriesIndex = 0
	albumListCursorIndex  = 1
)

// AlbumKind separates the three things this listing pair returns, because they need different
// words in front of the user: an album of their own, an album someone shared with them, and a
// bundle of loose shared photos that has no name to show.
type AlbumKind string

const (
	AlbumOwned  AlbumKind = "own"
	AlbumShared AlbumKind = "shared"
	AlbumBundle AlbumKind = "bundle"
)

type Album struct {
	ID        string
	Title     string
	ItemCount int
	CreatedAt time.Time
	CoverURL  string
	Kind      AlbumKind
	Owner     Person
}

// Person is whoever an album or bundle came from. The name is what the UI shows; the id is
// what tells the account holder apart from everyone else, since a display name is neither
// unique nor stable.
type Person struct {
	ID   string
	Name string
}

type AlbumPage struct {
	Albums    []Album
	NextToken string
}

// Albums returns one page of the album list. Pass the previous page's NextToken to
// continue; an empty NextToken in the result means the listing is exhausted.
func (c *Client) Albums(ctx context.Context, pageToken string) (AlbumPage, error) {
	payload, err := c.call(ctx, albumsRPC, []any{nullable(pageToken), nil, 2}, "/albums")
	if err != nil {
		return AlbumPage{}, err
	}
	return decodeAlbumPage(payload)
}

func decodeAlbumPage(payload any) (AlbumPage, error) {
	root := rootOf(payload, albumsRPC)

	entries, ok := root.at(albumListEntriesIndex).list()
	if !ok {
		return AlbumPage{}, root.at(albumListEntriesIndex).driftf("an array of albums")
	}

	page := AlbumPage{Albums: make([]Album, 0, len(entries))}
	for index := range entries {
		album, err := decodeAlbum(root.at(albumListEntriesIndex).at(index))
		if err != nil {
			return AlbumPage{}, err
		}
		page.Albums = append(page.Albums, album)
	}

	token, err := root.at(albumListCursorIndex).cursor()
	if err != nil {
		return AlbumPage{}, err
	}
	page.NextToken = token
	return page, nil
}

// decodeAlbum treats only the id as mandatory. The Phase 0 capture found untitled albums
// carry a null title, and a decoder that demanded one would reject a real, valid library.
func decodeAlbum(node tree) (Album, error) {
	id, ok := node.at(albumIDIndex).text()
	if !ok || id == "" {
		return Album{}, node.at(albumIDIndex).driftf("an album id")
	}

	album := Album{ID: id, Kind: AlbumOwned}
	album.Title, _ = node.at(albumTitleIndex).text()
	album.CoverURL, _ = node.at(albumCoverIndex).at(0).text()

	if kind, ok := node.at(albumKindIndex).number(); ok && int(kind) == bundleKind {
		album.Kind = AlbumBundle
	}

	if count, ok := node.at(albumItemCountIndex).number(); ok {
		album.ItemCount = int(count)
	}
	album.CreatedAt = millisToTime(node.at(albumCreatedAtIndex))
	album.Owner = decodePerson(node.at(albumOwnerIndex).at(0))
	return album, nil
}

// decodePerson reports whoever it can and stays silent otherwise. A missing owner costs a
// caption; refusing the whole listing over one would cost the user their albums.
func decodePerson(node tree) Person {
	var person Person
	person.ID, _ = node.at(personIDIndex).text()
	person.Name, _ = node.at(personNameIndex).at(0).text()
	return person
}

func millisToTime(node tree) time.Time {
	millis, ok := node.number()
	if !ok || millis <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(millis)).UTC()
}

// nullable keeps an absent page token out of the payload as JSON null rather than an empty
// string; the server treats the two differently on the first page.
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
