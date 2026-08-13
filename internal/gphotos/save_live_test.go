//go:build live

package gphotos

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLiveSaveSamples writes one still from each content host into GPB_SAMPLE_DIR so a human
// can open them and judge fidelity directly. It asserts nothing beyond the download
// succeeding: the point is the file, not the verdict.
func TestLiveSaveSamples(t *testing.T) {
	target := os.Getenv("GPB_SAMPLE_DIR")
	if target == "" {
		t.Skip("set GPB_SAMPLE_DIR to write sample downloads")
	}

	client := liveClient(t)
	ctx := t.Context()

	page, err := client.Albums(ctx, "")
	if err != nil {
		t.Fatalf("listing albums: %v", err)
	}

	saved := map[string]bool{}
	for _, album := range pickAlbumsToProbe(page.Albums, 4) {
		items, err := client.AlbumItems(ctx, album.ID, "")
		if err != nil || len(items.Items) == 0 {
			continue
		}

		for _, item := range items.Items {
			if item.IsVideo || len(saved) == len(contentHosts) {
				continue
			}

			signed, err := client.DownloadURL(ctx, item.MediaKey, album.ID)
			if err != nil {
				t.Fatalf("minting a URL: %v", err)
			}
			host := hostOf(t, signed)
			if saved[host] {
				continue
			}

			file, err := os.Create(filepath.Join(target, host+".jpg"))
			if err != nil {
				t.Fatalf("creating the sample file: %v", err)
			}
			download, err := client.Fetch(ctx, signed, 0, file)
			file.Close()
			if err != nil {
				t.Fatalf("fetching from %s: %v", host, err)
			}

			saved[host] = true
			t.Logf("%s -> %s.jpg: %d bytes, advertised %dx%d, google calls it %q",
				host, host, download.Size, item.Width, item.Height, download.Filename)
		}
	}

	if len(saved) == 0 {
		t.Fatal("nothing was saved")
	}
}
