package syncer

import (
	"path/filepath"
	"strings"
	"time"

	"gpb/internal/store"
)

// mediaKeyPrefixLength is how much of the media key prefixes each filename. It exists to make
// names collision-free without depending on Google's filenames, which repeat constantly —
// every phone in the world produces IMG_0001.JPG. Twelve base64 characters is 72 bits, far
// past the point where a personal library could collide.
const mediaKeyPrefixLength = 12

// unknownDateDir holds items with no usable date. They are still backed up; they just cannot
// be filed by month.
const unknownDateDir = "unknown"

// poolPath places an item at pool/<year>/<year>-<month>/<key>_<filename>, keyed on capture
// date so the layout survives album renames and re-organisation upstream.
func poolPath(root string, item store.MediaItem) string {
	return filepath.Join(root, dateDirs(item.CapturedAt), poolFilename(item))
}

func dateDirs(capturedAt time.Time) string {
	if capturedAt.IsZero() {
		return unknownDateDir
	}
	return filepath.Join(capturedAt.Format("2006"), capturedAt.Format("2006-01"))
}

func poolFilename(item store.MediaItem) string {
	prefix := safeName(item.MediaKey)
	if len(prefix) > mediaKeyPrefixLength {
		prefix = prefix[:mediaKeyPrefixLength]
	}

	name := safeName(item.Filename)
	if name == "" {
		return prefix
	}
	return prefix + "_" + name
}

// partPath is where a download accumulates before it is committed. It lives on the same
// filesystem as the pool so the commit can be a rename, and is keyed on the media key alone
// so an interrupted download is found again by the next run.
func partPath(tempDir, mediaKey string) string {
	return filepath.Join(tempDir, safeName(mediaKey)+".part")
}

// SafeName reduces a remote-supplied string to something that cannot escape its directory or
// confuse a shell. Google's filenames, media keys and album titles are all remote input, and
// all end up as path components. It is exported because the album link tree builds paths
// from the same untrusted strings, and two implementations of this would eventually disagree.
func SafeName(name string) string { return safeName(name) }

func safeName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7F:
			return -1
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		default:
			return r
		}
	}, name)

	name = strings.Trim(name, " .")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}
