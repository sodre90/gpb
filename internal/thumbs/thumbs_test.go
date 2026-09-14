package thumbs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource counts what actually reached Google, which is the property most of these tests
// are really about: the cache exists so a person browsing an album twice costs nothing.
type fakeSource struct {
	mu      sync.Mutex
	calls   int
	body    string
	err     error
	release chan struct{}
}

func (f *fakeSource) Thumbnail(ctx context.Context, baseURL string, w io.Writer) (int64, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if f.err != nil {
		return 0, f.err
	}

	written, err := io.WriteString(w, f.body)
	return int64(written), err
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newCache(t *testing.T, maxBytes int64) *Cache {
	t.Helper()
	return NewCache(filepath.Join(t.TempDir(), "thumbs"), maxBytes)
}

func TestASecondRequestIsServedFromDisk(t *testing.T) {
	cache := newCache(t, 1<<20)
	source := &fakeSource{body: "jpeg-bytes"}

	for range 3 {
		image, err := cache.Get(t.Context(), "AF1QipAAAA", "https://example/thumb", source)
		if err != nil {
			t.Fatalf("getting a thumbnail: %v", err)
		}
		if string(image) != "jpeg-bytes" {
			t.Fatalf("the cache served %q", image)
		}
	}

	if source.count() != 1 {
		t.Errorf("three requests reached Google %d times, want 1", source.count())
	}
}

// A fresh grid page is 200 near-simultaneous requests. If a browser renders the same album
// in two tabs, the same key must still cost Google one fetch.
func TestConcurrentMissesCollapseIntoOneFetch(t *testing.T) {
	cache := newCache(t, 1<<20)
	source := &fakeSource{body: "jpeg-bytes", release: make(chan struct{})}

	var waiting sync.WaitGroup
	results := make([][]byte, 8)
	for index := range results {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			image, err := cache.Get(t.Context(), "AF1QipAAAA", "https://example/thumb", source)
			if err != nil {
				t.Errorf("getting a thumbnail: %v", err)
				return
			}
			results[index] = image
		}()
	}

	// Let every goroutine arrive before the one real fetch is allowed to finish.
	time.Sleep(50 * time.Millisecond)
	close(source.release)
	waiting.Wait()

	if source.count() != 1 {
		t.Errorf("eight concurrent misses reached Google %d times, want 1", source.count())
	}
	for index, image := range results {
		if string(image) != "jpeg-bytes" {
			t.Errorf("waiter %d received %q", index, image)
		}
	}
}

// The browser lets go of a thumbnail's request when its cell scrolls away, and the same key
// may be asked for again a moment later by a cell that stayed. The first request's
// cancellation is its own; the second must still get the picture.
func TestAWaiterOutlivesALeaderThatWasCancelled(t *testing.T) {
	cache := newCache(t, 1<<20)
	source := &fakeSource{body: "jpeg-bytes", release: make(chan struct{})}

	leaderContext, cancelLeader := context.WithCancel(t.Context())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := cache.Get(leaderContext, "AF1QipAAAA", "https://example/thumb", source)
		leaderDone <- err
	}()
	time.Sleep(20 * time.Millisecond)

	waiterDone := make(chan []byte, 1)
	go func() {
		image, err := cache.Get(t.Context(), "AF1QipAAAA", "https://example/thumb", source)
		if err != nil {
			t.Errorf("the waiter failed: %v", err)
		}
		waiterDone <- image
	}()
	time.Sleep(20 * time.Millisecond)

	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled leader reported %v", err)
	}
	close(source.release)

	if image := <-waiterDone; string(image) != "jpeg-bytes" {
		t.Errorf("the waiter received %q", image)
	}
	if source.count() != 2 {
		t.Errorf("Google was asked %d times, want 2: the leader's abandoned fetch and the waiter's own", source.count())
	}
}

func TestAFailedFetchIsNotCached(t *testing.T) {
	cache := newCache(t, 1<<20)
	source := &fakeSource{err: errors.New("google said no")}

	if _, err := cache.Get(t.Context(), "AF1QipAAAA", "https://example/thumb", source); err == nil {
		t.Fatal("a failing fetch reported success")
	}

	source.err = nil
	source.body = "jpeg-bytes"
	image, err := cache.Get(t.Context(), "AF1QipAAAA", "https://example/thumb", source)
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if string(image) != "jpeg-bytes" {
		t.Fatalf("the retry served %q", image)
	}
}

// A media key is remote input that becomes a path. This is the case the hashing exists for.
func TestAHostileMediaKeyStaysInsideTheCache(t *testing.T) {
	root := t.TempDir()
	cache := NewCache(filepath.Join(root, "thumbs"), 1<<20)
	source := &fakeSource{body: "jpeg-bytes"}

	for _, key := range []string{"..", "../../etc/passwd", "/absolute", ""} {
		if key == "" {
			if _, err := cache.Get(t.Context(), key, "https://example/thumb", source); err == nil {
				t.Error("an empty media key was accepted")
			}
			continue
		}
		if _, err := cache.Get(t.Context(), key, "https://example/thumb", source); err != nil {
			t.Fatalf("caching %q: %v", key, err)
		}
	}

	escaped := 0
	filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && !strings.HasPrefix(path, filepath.Join(root, "thumbs")) {
			escaped++
		}
		return nil
	})
	if escaped != 0 {
		t.Errorf("%d files were written outside the cache directory", escaped)
	}
}

func TestSweepEvictsTheOldestFirst(t *testing.T) {
	cache := newCache(t, 40)
	source := &fakeSource{body: strings.Repeat("x", 10)}

	keys := []string{"oldest", "middle", "newest", "newer-still", "newest-of-all"}
	for index, key := range keys {
		if _, err := cache.Get(t.Context(), key, "https://example/thumb", source); err != nil {
			t.Fatalf("caching %s: %v", key, err)
		}
		// Distinct mtimes: the sweep orders by them, and same-millisecond writes would make
		// the assertion depend on filesystem timestamp resolution.
		age := time.Now().Add(time.Duration(index-len(keys)) * time.Hour)
		os.Chtimes(cache.pathFor(key), age, age)
	}

	if err := cache.Sweep(); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	if _, err := os.Stat(cache.pathFor("oldest")); !os.IsNotExist(err) {
		t.Errorf("the oldest entry survived the sweep: %v", err)
	}
	if _, err := os.Stat(cache.pathFor("newest-of-all")); err != nil {
		t.Errorf("the newest entry was evicted: %v", err)
	}
}

func TestSweepIsANoOpWhenTheCacheFits(t *testing.T) {
	cache := newCache(t, 1<<20)
	source := &fakeSource{body: "jpeg-bytes"}

	if _, err := cache.Get(t.Context(), "AF1QipAAAA", "https://example/thumb", source); err != nil {
		t.Fatalf("caching: %v", err)
	}
	if err := cache.Sweep(); err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if _, err := os.Stat(cache.pathFor("AF1QipAAAA")); err != nil {
		t.Errorf("a cache under its cap lost an entry: %v", err)
	}
}

// Reading a cached entry has to count as use, or the sweep evicts exactly the thumbnails
// someone is looking at right now.
func TestAHitRefreshesTheEntrysAge(t *testing.T) {
	cache := newCache(t, 1<<20)
	source := &fakeSource{body: "jpeg-bytes"}

	if _, err := cache.Get(t.Context(), "AF1QipAAAA", "https://example/thumb", source); err != nil {
		t.Fatalf("caching: %v", err)
	}

	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(cache.pathFor("AF1QipAAAA"), old, old)

	if _, err := cache.Get(t.Context(), "AF1QipAAAA", "https://example/thumb", source); err != nil {
		t.Fatalf("re-reading: %v", err)
	}

	info, err := os.Stat(cache.pathFor("AF1QipAAAA"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.ModTime().Before(time.Now().Add(-time.Minute)) {
		t.Errorf("a cache hit left the entry dated %s", info.ModTime())
	}
}

func TestAnEmptyResponseIsRefused(t *testing.T) {
	cache := newCache(t, 1<<20)
	source := &fakeSource{body: ""}

	if _, err := cache.Get(t.Context(), "AF1QipAAAA", "https://example/thumb", source); err == nil {
		t.Fatal("an empty thumbnail was accepted and cached")
	}
	if _, err := os.Stat(cache.pathFor("AF1QipAAAA")); !os.IsNotExist(err) {
		t.Error("an empty thumbnail reached the disk")
	}
}
