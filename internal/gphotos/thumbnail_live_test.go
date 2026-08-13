//go:build live

package gphotos

import (
	"bytes"
	"testing"
)

// TestLiveThumbnailFetch is the contract fixtures cannot cover: that a URL taken from a
// listing still serves a grid-sized image to a real session.
//
// Measured 2026-08-10 across 1586 items in 10 albums: every item carried a thumbnail URL,
// every one on photos.fife.usercontent.google.com with no query string, and every one 403s
// without the session cookies. Those three facts are what the allowlist, the stored URL and
// the cookie-carrying transport are each built on.
func TestLiveThumbnailFetch(t *testing.T) {
	client := liveClient(t)
	ctx := t.Context()

	page, err := client.Albums(ctx, "")
	if err != nil {
		t.Fatalf("listing albums: %v", err)
	}

	baseURL, isVideo := firstThumbnailURL(t, client, page.Albums)
	t.Logf("thumbnail URL on host %s, video %v", hostOf(t, baseURL), isVideo)

	var image bytes.Buffer
	written, err := client.Thumbnail(ctx, baseURL, &image)
	if err != nil {
		t.Fatalf("fetching the thumbnail: %v", err)
	}

	t.Logf("fetched %d bytes", written)
	if written != int64(image.Len()) {
		t.Errorf("reported %d bytes but wrote %d", written, image.Len())
	}
	if !bytes.HasPrefix(image.Bytes(), []byte{0xFF, 0xD8}) {
		t.Errorf("the thumbnail is not a JPEG (first bytes %x)", image.Bytes()[:min(4, image.Len())])
	}

	// A grid cell is 256 pixels. If this ever comes back at hundreds of kilobytes, the size
	// suffix has stopped being honoured and every page view got twenty times more expensive.
	if written > 200<<10 {
		t.Errorf("the grid thumbnail is %d bytes — the size suffix is not being honoured", written)
	}
}

// A URL from a listing is worthless if the session is not required to fetch it, because the
// whole thumbnail path is built on the app being the only party that holds cookies.
func TestLiveThumbnailsRefuseAnonymousFetches(t *testing.T) {
	client := liveClient(t)

	page, err := client.Albums(t.Context(), "")
	if err != nil {
		t.Fatalf("listing albums: %v", err)
	}
	baseURL, _ := firstThumbnailURL(t, client, page.Albums)

	anonymous := &Client{downloads: anonymousTransport, userAgent: client.userAgent}
	if _, err := anonymous.Thumbnail(t.Context(), baseURL, &bytes.Buffer{}); err == nil {
		t.Error("a thumbnail was served without the session — the cookie discipline is moot")
	} else {
		t.Logf("an anonymous fetch was refused: %v", err)
	}
}

func firstThumbnailURL(t *testing.T, client *Client, albums []Album) (string, bool) {
	t.Helper()

	for _, album := range albums {
		if album.ItemCount == 0 {
			continue
		}
		items, err := client.AlbumItems(t.Context(), album.ID, "")
		if err != nil {
			t.Fatalf("listing album items: %v", err)
		}
		for _, item := range items.Items {
			if item.ThumbnailURL != "" {
				return item.ThumbnailURL, item.IsVideo
			}
		}
	}

	t.Fatal("no item in the account carried a thumbnail URL")
	return "", false
}
