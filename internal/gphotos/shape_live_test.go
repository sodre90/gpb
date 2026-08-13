//go:build live

package gphotos

import (
	"bytes"
	"encoding/binary"
	"net/url"
	"strings"
	"testing"
)

// TestLiveDownloadResponseShape is a diagnostic, not an assertion: it prints where a live
// VrseUb answer keeps its URLs so a position change can be re-pinned without recapturing
// through DevTools. It prints hosts and lengths only, never a signature.
func TestLiveDownloadResponseShape(t *testing.T) {
	client := liveClient(t)
	ctx := t.Context()

	page, err := client.Albums(ctx, "")
	if err != nil {
		t.Fatalf("listing albums: %v", err)
	}

	for _, album := range pickAlbumsToProbe(page.Albums, 3) {
		items, err := client.AlbumItems(ctx, album.ID, "")
		if err != nil {
			t.Fatalf("listing items: %v", err)
		}
		if len(items.Items) == 0 {
			continue
		}

		item := items.Items[0]
		payload, err := client.call(ctx, downloadURLRPC,
			[]any{item.MediaKey, nil, nil, nil, album.ID}, "/photo/"+item.MediaKey)
		if err != nil {
			t.Fatalf("calling %s: %v", downloadURLRPC, err)
		}

		t.Logf("album of %d items, first item video=%v", album.ItemCount, item.IsVideo)
		describeURLPositions(t, payload)
	}
}

// TestLiveEveryItemDownloadsAtFullResolution guards the correction made on 2026-08-10.
//
// VrseUb mints URLs on two hosts, and this client briefly treated the photos.fife one as a
// display derivative and refused it — which would have silently skipped a third of a shared
// album. Both hosts in fact serve the camera's own file; fife merely demands the session on
// top of the URL signature. The proof is pixels: what arrives must match what the item
// listing advertises, so a regression to a downscaled derivative fails here rather than
// years later when someone opens the backup.
func TestLiveEveryItemDownloadsAtFullResolution(t *testing.T) {
	client := liveClient(t)
	ctx := t.Context()

	page, err := client.Albums(ctx, "")
	if err != nil {
		t.Fatalf("listing albums: %v", err)
	}

	stills, videos := 0, 0
	for _, album := range pickAlbumsToProbe(page.Albums, 3) {
		items, err := client.AlbumItems(ctx, album.ID, "")
		if err != nil || len(items.Items) == 0 {
			continue
		}

		for _, item := range sampleStillsAndVideos(items.Items, 3) {
			signed, err := client.DownloadURL(ctx, item.MediaKey, album.ID)
			if err != nil {
				t.Fatalf("minting a download URL: %v", err)
			}

			var buffer bytes.Buffer
			download, err := client.Fetch(ctx, signed, 0, &buffer)
			if err != nil {
				t.Fatalf("fetching from %s: %v", hostOf(t, signed), err)
			}
			if download.Filename == "" {
				t.Errorf("%s served no filename", hostOf(t, signed))
			}

			if item.IsVideo {
				videos++
				t.Logf("video from %s: %d bytes, %s", hostOf(t, signed), download.Size, download.ContentType)
				continue
			}

			stills++
			width, height := jpegDimensions(buffer.Bytes())
			if !samePixels(width, height, item.Width, item.Height) {
				t.Errorf("%s served %dx%d for an item advertised as %dx%d",
					hostOf(t, signed), width, height, item.Width, item.Height)
			}
		}
	}

	t.Logf("verified %d stills at full resolution and downloaded %d videos", stills, videos)
	if stills == 0 {
		t.Fatal("no still was checked — the download path is untested")
	}
}

// sampleStillsAndVideos takes up to `each` of both kinds. An album's newest entries are
// usually all photos, so taking the first few would leave videos — the case most likely to
// behave differently — untested.
func sampleStillsAndVideos(items []MediaItem, each int) []MediaItem {
	var stills, videos []MediaItem
	for _, item := range items {
		switch {
		case item.IsVideo && len(videos) < each:
			videos = append(videos, item)
		case !item.IsVideo && len(stills) < each:
			stills = append(stills, item)
		}
	}
	return append(stills, videos...)
}

// samePixels tolerates a transpose. The item listing reports display orientation while a
// JPEG's start-of-frame reports the sensor's own axes, so a portrait phone photo legitimately
// arrives as landscape pixels plus an Exif orientation tag.
func samePixels(width, height, advertisedWidth, advertisedHeight int) bool {
	return (width == advertisedWidth && height == advertisedHeight) ||
		(width == advertisedHeight && height == advertisedWidth)
}

// jpegDimensions walks the segment chain to the start-of-frame marker. Byte count alone
// cannot distinguish a downscale from a re-encode at the same pixel count.
func jpegDimensions(data []byte) (int, int) {
	for offset := 2; offset+9 < len(data); {
		if data[offset] != 0xFF {
			return 0, 0
		}
		marker := data[offset+1]
		length := int(binary.BigEndian.Uint16(data[offset+2:]))
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			height := int(binary.BigEndian.Uint16(data[offset+5:]))
			width := int(binary.BigEndian.Uint16(data[offset+7:]))
			return width, height
		}
		offset += 2 + length
	}
	return 0, 0
}

func describeURLPositions(t *testing.T, payload any) {
	t.Helper()

	elements, ok := payload.([]any)
	if !ok {
		t.Logf("  payload is %s", sampleShape(payload, 1))
		return
	}

	for index, element := range elements {
		text, ok := element.(string)
		if !ok || !strings.HasPrefix(text, "https://") {
			continue
		}
		parsed, err := url.Parse(text)
		if err != nil {
			continue
		}
		t.Logf("  [%d] host=%s length=%d", index, parsed.Host, len(text))
	}
}

func pickAlbumsToProbe(albums []Album, limit int) []Album {
	var picked []Album
	for _, album := range albums {
		if album.ItemCount == 0 {
			continue
		}
		picked = append(picked, album)
		if len(picked) == limit {
			break
		}
	}
	return picked
}
