package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpb/internal/store"
)

// held puts a real file in the pool and tells the store the item is downloaded, which is the
// only state in which there is anything to serve.
func held(t *testing.T, server *Server, key, name, contents string) string {
	t.Helper()

	pool := server.cfg.PoolDir()
	if err := os.MkdirAll(pool, 0o700); err != nil {
		t.Fatalf("making the pool: %v", err)
	}
	path := filepath.Join(pool, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}

	item := store.MediaItem{MediaKey: key, Filename: name, LocalPath: path, SizeBytes: int64(len(contents))}
	if err := server.store.MarkDownloaded(item, time.Now()); err != nil {
		t.Fatalf("marking %s downloaded: %v", key, err)
	}
	return path
}

func TestAnImageIsServedFromDiskRatherThanGoogle(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 1)
	held(t, server, "AF1Qip0000", "IMG_0000.jpg", "the whole photograph")

	recorder := get(handler, "/image/AF1Qip0000", login(t, handler))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /image returned %d, want 200", recorder.Code)
	}
	if body := recorder.Body.String(); body != "the whole photograph" {
		t.Errorf("served %q, want the file's contents", body)
	}
	if cache := recorder.Header().Get("Cache-Control"); !strings.HasPrefix(cache, "private") {
		t.Errorf("Cache-Control is %q, want a private cache", cache)
	}
}

// The viewer seeks through video, which only works if ranges are honoured.
func TestAnImageRequestCanAskForOneRange(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 1)
	held(t, server, "AF1Qip0000", "IMG_0000.mp4", "0123456789")

	request := httptest.NewRequest(http.MethodGet, "/image/AF1Qip0000", nil)
	request.AddCookie(login(t, handler))
	request.Header.Set("Range", "bytes=2-5")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("a ranged GET returned %d, want 206", recorder.Code)
	}
	if body := recorder.Body.String(); body != "2345" {
		t.Errorf("the range served %q, want 2345", body)
	}
}

func TestAnItemWithNoFileYetIsNotFound(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 1)

	if code := get(handler, "/image/AF1Qip0000", login(t, handler)).Code; code != http.StatusNotFound {
		t.Errorf("an item with no file returned %d, want 404", code)
	}
}

// The path comes from the database, so the handler treats it as an assertion to be checked
// rather than a fact. A row pointing outside the pool must open nothing.
func TestAStoredPathCannotReachOutsideThePool(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 1)

	secret := filepath.Join(server.cfg.PhotosDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("not a photo"), 0o600); err != nil {
		t.Fatalf("writing the decoy: %v", err)
	}
	held(t, server, "AF1Qip0000", "IMG_0000.jpg", "the whole photograph")
	escaping := store.MediaItem{MediaKey: "AF1Qip0000", Filename: "IMG_0000.jpg", LocalPath: secret}
	if err := server.store.MarkDownloaded(escaping, time.Now()); err != nil {
		t.Fatalf("pointing the row outside the pool: %v", err)
	}

	recorder := get(handler, "/image/AF1Qip0000", login(t, handler))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("an escaping path returned %d, want 404", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "not a photo") {
		t.Error("the handler served a file from outside the pool")
	}
}

func TestAnImageNeedsASession(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	seedAlbums(t, server, store.Album{ID: "holiday", Title: "Holiday 2026"})
	seedItems(t, server, "holiday", 1)
	held(t, server, "AF1Qip0000", "IMG_0000.jpg", "the whole photograph")

	if code := get(handler, "/image/AF1Qip0000", nil).Code; code != http.StatusSeeOther {
		t.Errorf("an unauthenticated GET returned %d, want a redirect to the login page", code)
	}
}
