//go:build live

// Live contract tests. They talk to the real Google Photos account behind GPB_DATA_DIR's
// browser profile, so they never run in CI:
//
//	go test -tags live ./internal/gphotos/ -v
//
// Their job is the one thing fixtures cannot do — prove the request shape we synthesise is
// still accepted, and that the download path still yields untouched originals.
//
// Nothing here may print a media key, an album title or a signed URL: this output ends up
// in a terminal, a scrollback buffer and possibly a bug report.
package gphotos

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"gpb/internal/auth"
)

func liveClient(t *testing.T) *Client {
	t.Helper()

	dataDir := os.Getenv("GPB_DATA_DIR")
	if dataDir == "" {
		t.Skip("set GPB_DATA_DIR to a data directory holding a signed-in profile")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	session, err := auth.NewManager(dataDir + "/profile").Warmup(ctx)
	if err != nil {
		t.Fatalf("warming up the profile: %v", err)
	}

	client, err := NewClient(session)
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	return client
}

func TestLiveAlbumListing(t *testing.T) {
	client := liveClient(t)
	ctx := t.Context()

	page, err := client.Albums(ctx, "")
	if err != nil {
		t.Fatalf("listing albums: %v", err)
	}
	if len(page.Albums) == 0 {
		t.Fatal("the account reported no albums at all")
	}
	t.Logf("page one carried %d albums, continuation token %v", len(page.Albums), page.NextToken != "")

	for _, album := range page.Albums {
		if album.ID == "" {
			t.Fatal("an album arrived without an id")
		}
	}

	if page.NextToken == "" {
		return
	}

	second, err := client.Albums(ctx, page.NextToken)
	if err != nil {
		t.Fatalf("listing the second album page: %v", err)
	}

	seen := make(map[string]bool, len(page.Albums))
	for _, album := range page.Albums {
		seen[album.ID] = true
	}
	for _, album := range second.Albums {
		if seen[album.ID] {
			t.Fatal("the second album page repeated an album from the first — paging is not advancing")
		}
	}
	t.Logf("page two carried %d albums with no overlap", len(second.Albums))
}

// The shared listing is what makes albums other people shared visible at all: measured
// 2026-08-10 it returned 18 albums, 3714 photos, that the album listing never mentions. It is
// also the only place their titles exist, so a refusal here is a silent loss of both.
func TestLiveSharedAlbumListing(t *testing.T) {
	client := liveClient(t)
	ctx := t.Context()

	page, err := client.SharedAlbums(ctx, "")
	if err != nil {
		t.Fatalf("listing shared albums: %v", err)
	}
	if len(page.Albums) == 0 {
		t.Fatal("the account reported no shared albums at all")
	}

	untitled := 0
	for _, album := range page.Albums {
		if album.ID == "" {
			t.Fatal("a shared album arrived without an id")
		}
		if album.Title == "" {
			untitled++
		}
	}
	t.Logf("page one carried %d shared albums, %d untitled, continuation token %v",
		len(page.Albums), untitled, page.NextToken != "")

	// The point of this listing is the titles. Losing them would leave the album list showing
	// placeholders for albums that do have names, which is the bug this call was added to fix.
	if untitled*2 > len(page.Albums) {
		t.Errorf("%d of %d shared albums decoded without a title", untitled, len(page.Albums))
	}

	if page.NextToken == "" {
		return
	}
	second, err := client.SharedAlbums(ctx, page.NextToken)
	if err != nil {
		t.Fatalf("listing the second shared album page: %v", err)
	}

	seen := make(map[string]bool, len(page.Albums))
	for _, album := range page.Albums {
		seen[album.ID] = true
	}
	for _, album := range second.Albums {
		if seen[album.ID] {
			t.Fatal("the second shared album page repeated an entry — paging is not advancing")
		}
	}
	t.Logf("page two carried %d shared albums with no overlap", len(second.Albums))
}

func TestLiveAlbumItemsAndOriginalDownload(t *testing.T) {
	client := liveClient(t)
	ctx := t.Context()

	page, err := client.Albums(ctx, "")
	if err != nil {
		t.Fatalf("listing albums: %v", err)
	}

	signed := mintForFirstStill(t, client, page.Albums)
	t.Logf("minted a %d-character signed URL on host %s", len(signed), hostOf(t, signed))

	var buffer bytes.Buffer
	download, err := client.Fetch(ctx, signed, 0, &buffer)
	if err != nil {
		t.Fatalf("fetching the signed URL: %v", err)
	}

	t.Logf("downloaded %d bytes, content-type %q, filename %d characters",
		download.Size, download.ContentType, len(download.Filename))

	if download.Size != int64(buffer.Len()) {
		t.Errorf("reported %d bytes but wrote %d", download.Size, buffer.Len())
	}
	if download.Filename == "" {
		t.Error("the response carried no filename")
	}
	assertOriginalJPEG(t, buffer.Bytes())
}

// assertOriginalJPEG checks for an APP1/Exif segment. The Phase 0 spike found the lh3 "=d"
// URL returns a JFIF/APP0 re-encode with GPS and camera model stripped — visually similar,
// forensically worthless. This is the assertion that catches us drifting onto that path.
func assertOriginalJPEG(t *testing.T, data []byte) {
	t.Helper()

	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		t.Logf("not a JPEG (first bytes %x) — skipping the EXIF check", data[:min(4, len(data))])
		return
	}

	if !bytes.Contains(data[:min(len(data), 4096)], []byte("Exif")) {
		t.Error("the downloaded JPEG has no Exif segment — this looks like a stripped re-encode, not the original")
	}
}

// hostOf lets a test say where a signed URL pointed without printing the signature itself.
func hostOf(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		return "unparseable"
	}
	return parsed.Host
}

func mintForFirstStill(t *testing.T, client *Client, albums []Album) string {
	t.Helper()

	for _, album := range albums {
		if album.ItemCount == 0 {
			continue
		}

		items, err := client.AlbumItems(t.Context(), album.ID, "")
		if err != nil {
			t.Fatalf("listing album items: %v", err)
		}
		t.Logf("album of %d items listed %d entries", album.ItemCount, len(items.Items))

		for _, item := range items.Items {
			if item.IsVideo {
				continue
			}
			signed, err := client.DownloadURL(t.Context(), item.MediaKey, album.ID)
			if err != nil {
				t.Fatalf("minting a signed download URL: %v", err)
			}
			return signed
		}
	}

	t.Fatal("the account holds no still to download")
	return ""
}
