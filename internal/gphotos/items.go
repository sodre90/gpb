package gphotos

import (
	"context"
	"time"
)

const albumItemsRPC = "snAcKc"

// Positions inside one media entry, measured against the Phase 0 captures.
const (
	itemMediaKeyIndex   = 0
	itemThumbnailIndex  = 1
	itemCapturedAtIndex = 2
	itemDedupKeyIndex   = 3
	itemTimezoneIndex   = 4
	itemAddedAtIndex    = 5
	itemMetadataIndex   = 9
)

const (
	itemPageEntriesIndex   = 1
	itemPageCursorIndex    = 2
	itemPageAlbumMetaIndex = 3
)

// videoMetadataKey appears in an item's metadata map only for videos, where it carries
// duration and pixel dimensions. Its absence is how a still is recognised.
const videoMetadataKey = "76647426"

const videoDurationIndex = 0

type MediaItem struct {
	MediaKey       string
	ThumbnailURL   string
	Width          int
	Height         int
	CapturedAt     time.Time
	AddedAt        time.Time
	TimezoneOffset time.Duration
	DedupKey       string
	IsVideo        bool
	Duration       time.Duration
}

type ItemPage struct {
	Items      []MediaItem
	NextToken  string
	AlbumID    string
	AlbumTitle string
}

// AlbumItems returns one page of an album's contents. Google pages these at roughly 200
// entries; pass the previous NextToken to continue.
func (c *Client) AlbumItems(ctx context.Context, albumID, pageToken string) (ItemPage, error) {
	payload, err := c.call(ctx, albumItemsRPC, []any{albumID, nullable(pageToken)}, "/album/"+albumID)
	if err != nil {
		return ItemPage{}, err
	}
	return decodeItemPage(payload)
}

func decodeItemPage(payload any) (ItemPage, error) {
	root := rootOf(payload, albumItemsRPC)

	items, err := decodeMediaItems(root.at(itemPageEntriesIndex))
	if err != nil {
		return ItemPage{}, err
	}

	page := ItemPage{Items: items}
	if page.NextToken, err = root.at(itemPageCursorIndex).cursor(); err != nil {
		return ItemPage{}, err
	}
	page.AlbumID, _ = root.at(itemPageAlbumMetaIndex).at(0).text()
	page.AlbumTitle, _ = root.at(itemPageAlbumMetaIndex).at(1).text()
	return page, nil
}

// decodeMediaItems reads the array of entries an album page and a timeline page both carry,
// differing only in where they keep it.
func decodeMediaItems(node tree) ([]MediaItem, error) {
	entries, ok := node.list()
	if !ok {
		return nil, node.driftf("an array of media items")
	}

	items := make([]MediaItem, 0, len(entries))
	for index := range entries {
		item, err := decodeMediaItem(node.at(index))
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// decodeMediaItem requires only the media key. Everything else is enrichment: an item with
// no capture date still has to be backed up, it just lands in the pool's unknown bucket.
func decodeMediaItem(node tree) (MediaItem, error) {
	mediaKey, ok := node.at(itemMediaKeyIndex).text()
	if !ok || mediaKey == "" {
		return MediaItem{}, node.at(itemMediaKeyIndex).driftf("a media key")
	}

	item := MediaItem{MediaKey: mediaKey}
	item.ThumbnailURL, _ = node.at(itemThumbnailIndex).at(0).text()
	if width, ok := node.at(itemThumbnailIndex).at(1).number(); ok {
		item.Width = int(width)
	}
	if height, ok := node.at(itemThumbnailIndex).at(2).number(); ok {
		item.Height = int(height)
	}

	item.CapturedAt = millisToTime(node.at(itemCapturedAtIndex))
	item.AddedAt = millisToTime(node.at(itemAddedAtIndex))
	item.DedupKey, _ = node.at(itemDedupKeyIndex).text()
	if offset, ok := node.at(itemTimezoneIndex).number(); ok {
		item.TimezoneOffset = time.Duration(offset) * time.Millisecond
	}

	video := node.at(itemMetadataIndex).key(videoMetadataKey)
	if video.present() {
		item.IsVideo = true
		if millis, ok := video.at(videoDurationIndex).number(); ok {
			item.Duration = time.Duration(millis) * time.Millisecond
		}
	}
	return item, nil
}

// LocalCaptureTime renders the capture instant in the timezone the camera was in, which is
// what the pool's date bucketing should follow: a photo taken at 23:00 in Mallorca belongs
// in that day, not the next one in UTC.
func (m MediaItem) LocalCaptureTime() time.Time {
	if m.CapturedAt.IsZero() {
		return time.Time{}
	}
	return m.CapturedAt.In(time.FixedZone("", int(m.TimezoneOffset.Seconds())))
}
