package web

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpb/internal/store"
	"gpb/internal/syncer"
)

// TestPreview serves the real handlers against a scratch store on localhost, so the redesign can
// be looked at in a browser without touching the deployed daemon. It is a development harness,
// not a test: it blocks until interrupted and skips unless GPB_PREVIEW asks for it.
func TestPreview(t *testing.T) {
	address := os.Getenv("GPB_PREVIEW")
	if address == "" {
		t.Skip("set GPB_PREVIEW=127.0.0.1:8099 to serve the UI for a look")
	}

	server, _ := testServer(t)
	seedPreview(t, server)

	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listening on %s: %v", address, err)
	}
	fmt.Printf("\n  preview on http://%s/  password: %s\n\n", address, testPassword)
	log.Println(http.Serve(listener, server.Handler()))
}

func seedPreview(t *testing.T, server *Server) {
	t.Helper()

	now := time.Now()
	seedAlbums(t, server,
		store.Album{ID: "iceland", Title: "Iceland 2024", ItemCount: 843,
			CreatedAt: now.AddDate(-1, -5, 0)},
		store.Album{ID: "lake", Title: "Lake weekend", ItemCount: 130,
			CreatedAt: now.AddDate(-2, -7, 0)},
		store.Album{ID: "graduation", Title: "Graduation", ItemCount: 64,
			CreatedAt: now.AddDate(-6, -2, 0)},
		store.Album{ID: "shared", Title: "Trip with friends", ItemCount: 49,
			Kind: store.AlbumShared, OwnerName: "A friend", CreatedAt: now.AddDate(-1, 0, 0)},
	)
	// UpsertAlbum ignores the struct's SyncMode by design, so that a refresh from Google cannot
	// undo what the user chose. The mode has to be set through its own call.
	for albumID, mode := range map[string]store.SyncMode{
		"iceland": store.SyncPicked, "lake": store.SyncAll,
	} {
		if err := server.store.SetAlbumSyncMode(albumID, mode); err != nil {
			t.Fatalf("setting the preview sync mode for %s: %v", albumID, err)
		}
	}

	keys := seedItems(t, server, "iceland", 40)
	if err := server.store.SetSelection(keys[:24], true); err != nil {
		t.Fatalf("selecting preview items: %v", err)
	}
	for index, key := range keys[:14] {
		path, size := writePreviewPhoto(t, server, index)
		downloaded := store.MediaItem{
			MediaKey:  key,
			Filename:  fmt.Sprintf("IMG_%04d.jpg", index),
			LocalPath: path,
			SizeBytes: size,
			MimeType:  "image/jpeg",
		}
		if err := server.store.MarkDownloaded(downloaded, now); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}
	if err := server.store.MarkFailed(keys[15], errPreviewDownload); err != nil {
		t.Fatalf("marking a preview item failed: %v", err)
	}
	for _, key := range keys[16:18] {
		if err := server.store.MarkMissingUpstream(key, now); err != nil {
			t.Fatalf("marking %s missing: %v", key, err)
		}
	}
	if err := server.store.FlagForReview(keys[24:30]); err != nil {
		t.Fatalf("flagging preview arrivals for review: %v", err)
	}

	seedItems(t, server, "lake", 12)
	seedPreviewRuns(t, server, now)
}

var errPreviewDownload = errors.New("unexpected status 429 from the media host")

// writePreviewPhoto puts a real file in the pool. The viewer shows what is on disk, so a row
// pointing at nothing would preview the thumbnail fallback rather than the thing being looked at.
func writePreviewPhoto(t *testing.T, server *Server, index int) (string, int64) {
	t.Helper()

	canvas := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	for y := range canvas.Bounds().Dy() {
		for x := range canvas.Bounds().Dx() {
			canvas.Set(x, y, color.RGBA{uint8(x/5 + index*17), uint8(y / 4), uint8(index * 37), 255})
		}
	}

	path := filepath.Join(server.cfg.PoolDir(), fmt.Sprintf("2024/06/IMG_%04d.jpg", index))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("making the preview pool: %v", err)
	}

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	defer file.Close()

	if err := jpeg.Encode(file, canvas, nil); err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("measuring %s: %v", path, err)
	}
	return path, info.Size()
}

func seedPreviewRuns(t *testing.T, server *Server, now time.Time) {
	t.Helper()

	finished := []store.SyncRun{
		{StartedAt: now.Add(-3 * time.Hour), Outcome: store.OutcomeOK,
			Listed: 4211, Downloaded: 122, Bytes: 1_288_490_188},
		{StartedAt: now.Add(-27 * time.Hour), Outcome: store.OutcomePartial, Listed: 4198,
			Downloaded: 87, Failed: 3, Bytes: 838_860_800,
			Error: "3 downloads failed after retries: unexpected status 429 from the media host"},
		{StartedAt: now.Add(-51 * time.Hour), Outcome: store.OutcomeAuthRequired, Listed: 0},
	}
	for _, run := range finished {
		id, err := server.store.StartRun(run.StartedAt)
		if err != nil {
			t.Fatalf("starting a preview run: %v", err)
		}
		run.ID = id
		if err := server.store.FinishRun(run, run.StartedAt.Add(19*time.Minute)); err != nil {
			t.Fatalf("finishing a preview run: %v", err)
		}
	}

	if _, err := server.store.StartRun(now.Add(-6 * time.Minute)); err != nil {
		t.Fatalf("starting the live preview run: %v", err)
	}
	runs := runsOf(server)
	runs.activity = "backing up “Iceland 2024”"
	runs.progress = syncer.Progress{Listed: 1204, Downloaded: 350, Failed: 2}
}
