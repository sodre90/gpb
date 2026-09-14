// Package thumbs is the grid's image supply: a disk cache of small JPEGs in front of
// Google. A thumbnail is immutable for a media key, so an item fetched once is free
// forever, and browsing an album a second time costs Google nothing at all.
//
// The cache is entirely disposable. Deleting the directory costs re-fetches and nothing
// else, which is why it holds no index table and needs no consistency with the store.
package thumbs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Fetcher is the upstream half, declared here because this is where it is consumed.
// *gphotos.Client satisfies it.
type Fetcher interface {
	Thumbnail(ctx context.Context, baseURL string, w io.Writer) (int64, error)
}

// sweepSlack is how far below the cap a sweep cuts. Evicting exactly to the limit would make
// the next write sweep again; taking a tenth off means a sweep is a rare event.
const sweepSlack = 10

const (
	shardWidth = 2
	fileSuffix = ".jpg"
)

type Cache struct {
	dir      string
	maxBytes int64

	mu      sync.Mutex
	waiting map[string]*fetch

	// sweeping keeps concurrent writers from all deciding to evict at once. A sweep walks the
	// whole cache directory, and 200 grid cells missing together would otherwise start 200.
	sweeping sync.Mutex
	dirty    bool
}

// fetch is one in-flight upstream request that later arrivals for the same key wait on
// instead of duplicating. A fresh grid page is 200 near-simultaneous requests, and a browser
// that renders the same album twice must not double them.
type fetch struct {
	done  chan struct{}
	bytes []byte
	err   error
}

func NewCache(dir string, maxBytes int64) *Cache {
	return &Cache{dir: dir, maxBytes: maxBytes, waiting: map[string]*fetch{}}
}

// Get returns the image bytes for one media key, fetching and caching them on a miss.
func (c *Cache) Get(ctx context.Context, mediaKey, baseURL string, fetcher Fetcher) ([]byte, error) {
	if mediaKey == "" {
		return nil, errors.New("a thumbnail was requested without a media key")
	}

	if cached, err := c.read(mediaKey); err == nil {
		return cached, nil
	}
	return c.fetchOnce(ctx, mediaKey, baseURL, fetcher)
}

// fetchOnce collapses concurrent misses for one key into a single upstream request. The
// request that arrived first carries the fetch on its own context, and a browser abandons
// grid images freely — a cell scrolled away lets its request go — so a waiter whose leader
// was cancelled takes the fetch up itself rather than reporting the leader's cancellation as
// its own.
func (c *Cache) fetchOnce(ctx context.Context, mediaKey, baseURL string, fetcher Fetcher) ([]byte, error) {
	for {
		c.mu.Lock()
		existing, found := c.waiting[mediaKey]
		if !found {
			break
		}
		c.mu.Unlock()
		select {
		case <-existing.done:
			if !errors.Is(existing.err, context.Canceled) {
				return existing.bytes, existing.err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	pending := &fetch{done: make(chan struct{})}
	c.waiting[mediaKey] = pending
	c.mu.Unlock()

	pending.bytes, pending.err = c.fetchAndStore(ctx, mediaKey, baseURL, fetcher)
	close(pending.done)

	c.mu.Lock()
	delete(c.waiting, mediaKey)
	c.mu.Unlock()

	return pending.bytes, pending.err
}

func (c *Cache) fetchAndStore(ctx context.Context, mediaKey, baseURL string, fetcher Fetcher) ([]byte, error) {
	var buffer bytes.Buffer
	if _, err := fetcher.Thumbnail(ctx, baseURL, &buffer); err != nil {
		return nil, err
	}
	if buffer.Len() == 0 {
		return nil, errors.New("google returned an empty thumbnail")
	}

	if err := c.write(mediaKey, buffer.Bytes()); err != nil {
		// A cache that cannot be written is a performance problem, not a correctness one: the
		// image in hand is still the image the browser asked for.
		log.Printf("thumbs: caching a thumbnail: %v", err)
	}
	return buffer.Bytes(), nil
}

// read serves a hit and bumps the file's mtime, which is the whole of the LRU bookkeeping.
// atime would be the natural choice and is untrustworthy under relatime.
func (c *Cache) read(mediaKey string) ([]byte, error) {
	path := c.pathFor(mediaKey)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		log.Printf("thumbs: refreshing the age of %s: %v", mediaKey, err)
	}
	return data, nil
}

func (c *Cache) write(mediaKey string, data []byte) error {
	path := c.pathFor(mediaKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	// Written to a temporary name and renamed so a concurrent reader never sees a half file.
	temporary := path + ".part"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return err
	}

	c.sweepLater()
	return nil
}

// sweepLater runs eviction off the request path. A miss should cost the browser one fetch,
// not a walk of the whole cache directory.
func (c *Cache) sweepLater() {
	if !c.sweeping.TryLock() {
		return
	}
	go func() {
		defer c.sweeping.Unlock()
		if err := c.Sweep(); err != nil {
			log.Printf("thumbs: sweeping the cache: %v", err)
		}
	}()
}

type cached struct {
	path     string
	size     int64
	modified time.Time
}

// Sweep evicts oldest-first until the cache is comfortably under its cap. It is exported so
// a test can run it deterministically rather than racing the background trigger.
func (c *Cache) Sweep() error {
	if c.maxBytes <= 0 {
		return nil
	}

	entries, total, err := c.contents()
	if err != nil || total <= c.maxBytes {
		return err
	}

	slices.SortFunc(entries, func(a, b cached) int { return a.modified.Compare(b.modified) })

	target := c.maxBytes - c.maxBytes/sweepSlack
	removed := 0
	for _, entry := range entries {
		if total <= target {
			break
		}
		if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
			log.Printf("thumbs: evicting %s: %v", filepath.Base(entry.path), err)
			continue
		}
		total -= entry.size
		removed++
	}

	log.Printf("thumbs: evicted %d thumbnails, cache now %d bytes", removed, total)
	return nil
}

func (c *Cache) contents() ([]cached, int64, error) {
	var entries []cached
	var total int64

	err := filepath.WalkDir(c.dir, func(path string, entry os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir() || filepath.Ext(path) != fileSuffix:
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, cached{path: path, size: info.Size(), modified: info.ModTime()})
		total += info.Size()
		return nil
	})
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	return entries, total, err
}

// pathFor names the cache file after a hash of the media key, sharded on the first byte so
// no single directory holds a hundred thousand files.
//
// Hashing rather than sanitising: a media key is remote input that becomes a path, and every
// sanitiser has an escape nobody thought of — a key of ".." with the obvious character
// mapping walks straight out of the cache directory. A hex digest cannot express a path at
// all, is fixed-width, and shards evenly for free.
func (c *Cache) pathFor(mediaKey string) string {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(mediaKey)))
	return filepath.Join(c.dir, digest[:shardWidth], digest+fileSuffix)
}
