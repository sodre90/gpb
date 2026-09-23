package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gpb/internal/gphotos"
	"gpb/internal/store"
)

type downloaded struct {
	Path        string
	Size        int64
	SHA256      string
	ContentType string
	Filename    string
	Resumed     bool
}

// fetch runs the write protocol for one item: stream into a .part file on the pool's own
// filesystem, hash while streaming, fsync, then rename into place. The rename is the commit
// — it is the only step that makes the file visible in the pool, and it is atomic, so a
// crash can leave a partial download but never a partial pool entry.
func (s *Syncer) fetch(ctx context.Context, item store.MediaItem, albumID string, flight *inFlightItem) (downloaded, error) {
	part := partPath(s.tempDir, item.MediaKey)
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		return downloaded{}, fmt.Errorf("preparing the staging directory: %w", err)
	}

	result, err := s.stream(ctx, item, albumID, part, flight)
	if errors.Is(err, gphotos.ErrRangeIgnored) {
		// The server sent the whole file when asked to resume. Appending would interleave two
		// copies, so the partial is discarded and the item restarts from zero.
		if removeErr := os.Remove(part); removeErr != nil {
			return downloaded{}, fmt.Errorf("discarding an unusable partial: %w", removeErr)
		}
		result, err = s.stream(ctx, item, albumID, part, flight)
	}
	if err != nil {
		return downloaded{}, err
	}

	final := poolPath(s.poolDir, withFilename(item, result.Filename))
	if !s.commitSharingAHeldCopy(item, result, part, final) {
		if err := commit(part, final); err != nil {
			return downloaded{}, err
		}
	}

	result.Path = final
	return result, nil
}

// withFilename prefers the name Google puts on the download over the one in the listing:
// the Content-Disposition carries the camera's own filename and its real extension, which
// matters for items whose listing name is generic.
func withFilename(item store.MediaItem, served string) store.MediaItem {
	if served != "" {
		item.Filename = served
	}
	return item
}

func (s *Syncer) stream(ctx context.Context, item store.MediaItem, albumID, part string, flight *inFlightItem) (downloaded, error) {
	offset, digest, err := resumeFrom(part)
	if err != nil {
		return downloaded{}, err
	}

	file, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return downloaded{}, fmt.Errorf("opening the staging file: %w", err)
	}
	defer file.Close()

	download, err := s.source.DownloadOriginal(ctx, item.MediaKey, albumID, offset,
		flight.streaming(offset, io.MultiWriter(file, digest)))
	if err != nil {
		return downloaded{}, err
	}

	// fsync before the rename, so the commit cannot expose a file whose bytes are still only
	// in the page cache. Without it a power cut could leave a correctly-named empty file,
	// which is worse than no file: nothing would ever retry it.
	if err := file.Sync(); err != nil {
		return downloaded{}, fmt.Errorf("flushing the staging file: %w", err)
	}

	return downloaded{
		Size:        download.Size,
		SHA256:      hex.EncodeToString(digest.Sum(nil)),
		ContentType: download.ContentType,
		Filename:    download.Filename,
		Resumed:     offset > 0,
	}, nil
}

// resumeFrom reports how much of a previous attempt survived and returns a hash primed with
// those bytes. Re-reading the partial costs one local pass and is what lets the hash cover
// the whole file rather than only the resumed tail.
func resumeFrom(part string) (int64, hash.Hash, error) {
	digest := sha256.New()

	file, err := os.Open(part)
	if errors.Is(err, os.ErrNotExist) {
		return 0, digest, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("inspecting the staging file: %w", err)
	}
	defer file.Close()

	size, err := io.Copy(digest, file)
	if err != nil {
		return 0, nil, fmt.Errorf("rehashing the staging file: %w", err)
	}
	return size, digest, nil
}

// commit publishes a finished download. The directory fsync matters as much as the file one:
// on most filesystems a rename is only durable once its parent directory has been flushed.
func commit(part, final string) error {
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return fmt.Errorf("preparing the pool directory: %w", err)
	}
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("committing the download: %w", err)
	}
	return syncDir(filepath.Dir(final))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening the pool directory: %w", err)
	}
	defer dir.Close()

	if err := dir.Sync(); err != nil {
		return fmt.Errorf("flushing the pool directory: %w", err)
	}
	return nil
}

// discardStaleParts clears what a previous process left in the staging directory: partial
// downloads for items that are no longer pending, and links it had made and not yet renamed into
// the pool. A .part for an item still queued is kept deliberately: that is the resume path.
func discardStaleParts(tempDir string, keep map[string]bool) (int, error) {
	entries, err := os.ReadDir(tempDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the staging directory: %w", err)
	}

	discarded := 0
	for _, entry := range entries {
		name := entry.Name()
		if !isStale(name, keep) {
			continue
		}
		if err := os.Remove(filepath.Join(tempDir, name)); err != nil {
			return discarded, fmt.Errorf("discarding a stale leftover: %w", err)
		}
		discarded++
	}
	return discarded, nil
}

// isStale holds for every staged link because only a pass holding the run lock stages one, and
// the staging directory is tidied under that lock before anything is staged.
func isStale(name string, keep map[string]bool) bool {
	switch filepath.Ext(name) {
	case ".part":
		return !keep[strings.TrimSuffix(name, ".part")]
	case stagedLinkExtension:
		return true
	default:
		return false
	}
}
