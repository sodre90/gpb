package gphotos

import "context"

// sharedAlbumsRPC is the listing behind the albums page's "shared with me" filter. It exists
// here because the album listing alone is not the library: measured 2026-08-10, this call
// returned all 132 albums the album listing knows, with identical ids, titles, item counts and
// creation dates — plus 18 albums shared with this account, holding 3714 photos, that the
// album listing never mentions at all.
const sharedAlbumsRPC = "Z5xsfc"

// Positions inside one shared-album entry. The record sits in a single-key object at the end
// of the entry rather than at a fixed index: entries run to 12 slots for an album this account
// owns and 9 for one shared with it.
const (
	sharedIDIndex    = 0
	sharedCoverIndex = 1

	sharedTitleIndex     = 1
	sharedTimesIndex     = 2
	sharedItemCountIndex = 3

	// sharedCreatedAtIndex is a position inside the record's times array. Slots 0 and 1 there
	// are the span of capture dates, which is a different thing entirely; this one matched the
	// album listing's own creation date for all 132 albums both listings return.
	sharedCreatedAtIndex = 4
)

// SharedAlbums returns one page of albums shared with this account, alongside the ones it
// owns. Pass the previous page's NextToken to continue.
func (c *Client) SharedAlbums(ctx context.Context, pageToken string) (AlbumPage, error) {
	payload, err := c.call(ctx, sharedAlbumsRPC,
		[]any{nullable(pageToken), nil, nil, nil, 1, nil, nil, sharedPageSize, nil, 5}, "/albums")
	if err != nil {
		return AlbumPage{}, err
	}
	return decodeSharedAlbumPage(payload)
}

// sharedPageSize is what the web UI asks for. The server returns fewer and pages the rest, so
// this is an upper bound rather than a promise.
const sharedPageSize = 100

func decodeSharedAlbumPage(payload any) (AlbumPage, error) {
	root := rootOf(payload, sharedAlbumsRPC)

	entries, ok := root.at(albumListEntriesIndex).list()
	if !ok {
		return AlbumPage{}, root.at(albumListEntriesIndex).driftf("an array of shared albums")
	}

	page := AlbumPage{Albums: make([]Album, 0, len(entries))}
	for index := range entries {
		album, err := decodeSharedAlbum(root.at(albumListEntriesIndex).at(index))
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

// decodeSharedAlbum leaves Kind unset. Whether one of these is the caller's own album or one
// shared with them is not in the response — every entry carries the same marker — so it is
// decided by which listing already knew the id, which only the caller can see.
func decodeSharedAlbum(node tree) (Album, error) {
	id, ok := node.at(sharedIDIndex).text()
	if !ok || id == "" {
		return Album{}, node.at(sharedIDIndex).driftf("a shared album id")
	}

	record := node.last().only()
	if !record.present() {
		return Album{}, record.driftf("a shared album record")
	}

	album := Album{ID: id}
	album.Title, _ = record.at(sharedTitleIndex).text()
	album.CoverURL, _ = node.at(sharedCoverIndex).at(0).text()
	if count, ok := record.at(sharedItemCountIndex).number(); ok {
		album.ItemCount = int(count)
	}
	album.CreatedAt = millisToTime(record.at(sharedTimesIndex).at(sharedCreatedAtIndex))
	return album, nil
}
