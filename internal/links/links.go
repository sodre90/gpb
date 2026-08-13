// Package links maintains a browsable, album-shaped view of the pool: one directory per
// followed album, one symlink per downloaded item, named as the camera named it.
//
// The tree holds nothing that is not derivable from the store, which is what makes it safe
// to rebuild wholesale after every run. The pool plus the database remains the backup; this
// is a convenience for looking at it.
package links

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gpb/internal/store"
	"gpb/internal/syncer"
)

const (
	// DirName is the album view's root, a sibling of the pool inside the photos directory.
	DirName = "albums"

	untitledAlbumDir = "untitled"

	// disambiguatorLength is how much media key is spliced into a filename that collides with
	// another in the same album. Two phones both producing IMG_2041.HEIC is ordinary; nine
	// base64 characters is more than enough to separate them inside one album.
	disambiguatorLength = 9
)

type Report struct {
	Albums  int
	Links   int
	Removed int
	Skipped int
}

func (r Report) String() string {
	return fmt.Sprintf("%d albums, %d links, %d stale removed, %d left alone",
		r.Albums, r.Links, r.Removed, r.Skipped)
}

// rebuilding serialises the whole tree. Rebuild is asked for by the end of a run, by the
// scheduler and by the CLI, and two at once fight over the same directories: one removes a link
// the other has just decided to keep, and both then try to create it. A package-level lock is
// enough because a process has one album view.
var rebuilding sync.Mutex

// Rebuild makes <photosDir>/albums match the followed albums. Albums that have downloaded
// nothing yet still get a directory: an empty folder appearing the moment you follow an
// album is informative, where a missing one just looks broken.
//
// One album failing does not stop the others. The tree is a convenience over a backup that is
// already safe on disk, so refusing to rebuild 180 albums because something unexpected is
// sitting in the 181st gets the trade backwards. What could not be done comes back as an error
// naming each album, after everything that could be done has been.
func Rebuild(db *store.Store, photosDir string) (Report, error) {
	rebuilding.Lock()
	defer rebuilding.Unlock()

	albums, err := db.FollowedAlbums()
	if err != nil {
		return Report{}, err
	}

	root := filepath.Join(photosDir, DirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Report{}, fmt.Errorf("preparing the album view: %w", err)
	}

	report := Report{}
	wanted := make(map[string]bool, len(albums))
	var failures []error

	for _, album := range albums {
		directory := uniqueDirName(album, wanted)
		wanted[directory] = true

		if err := writeOneAlbum(db, root, directory, album.ID, &report); err != nil {
			failures = append(failures, fmt.Errorf("the %s album view: %w", directory, err))
			continue
		}
		report.Albums++
	}

	if err := prune(root, wanted, &report); err != nil {
		failures = append(failures, err)
	}
	return report, errors.Join(failures...)
}

func writeOneAlbum(db *store.Store, root, directory, albumID string, report *Report) error {
	items, err := db.DownloadedInAlbum(albumID)
	if err != nil {
		return err
	}
	return writeAlbum(filepath.Join(root, directory), items, report)
}

// uniqueDirName keeps two albums of the same name apart. Google allows duplicate titles, and
// silently merging two albums into one folder would misrepresent the library.
func uniqueDirName(album store.Album, taken map[string]bool) string {
	name := syncer.SafeName(album.Title)
	if name == "" {
		name = untitledAlbumDir
	}
	if !taken[name] {
		return name
	}
	return name + "_" + shortKey(album.ID)
}

func shortKey(key string) string {
	safe := syncer.SafeName(key)
	if len(safe) > disambiguatorLength {
		return safe[:disambiguatorLength]
	}
	return safe
}

// writeAlbum reconciles one directory rather than emptying and refilling it, so a rebuild
// during a browse does not make every file blink out of existence.
func writeAlbum(directory string, items []store.MediaItem, report *Report) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("preparing an album directory: %w", err)
	}

	wanted, err := plan(directory, items)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if entry.Type()&os.ModeSymlink == 0 {
			// Something here is not ours. Leaving the file alone was never in doubt; the name has
			// to be given up with it, because a link this rebuild still wanted to make under that
			// name would fail to be created and take every album after it down too.
			delete(wanted, entry.Name())
			report.Skipped++
			continue
		}

		target, wantedHere := wanted[entry.Name()]
		if wantedHere && sameTarget(path, target) {
			delete(wanted, entry.Name())
			report.Links++
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if !wantedHere {
			report.Removed++
		}
	}

	for name, target := range wanted {
		if err := os.Symlink(target, filepath.Join(directory, name)); err != nil {
			return fmt.Errorf("linking an album item: %w", err)
		}
		report.Links++
	}
	return nil
}

// plan maps link name to link target for one album. Targets are relative so the whole photos
// directory can be moved, or bind-mounted at a different path inside a container, without
// every link in the tree dangling — which is exactly what absolute targets would do here,
// since the daemon writes them as /photos/... and the host browses them as /srv/gpb/photos/...
func plan(directory string, items []store.MediaItem) (map[string]string, error) {
	wanted := make(map[string]string, len(items))
	used := make(map[string]bool, len(items))

	for _, item := range items {
		target, err := filepath.Rel(directory, item.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("relating an item to its album: %w", err)
		}
		wanted[linkName(item, used)] = target
	}
	return wanted, nil
}

func linkName(item store.MediaItem, used map[string]bool) string {
	name := syncer.SafeName(item.Filename)
	if name == "" {
		name = syncer.SafeName(item.MediaKey)
	}
	if used[name] {
		name = disambiguate(name, item.MediaKey)
	}

	used[name] = true
	return name
}

// disambiguate splices the media key in before the extension, so a collided file still opens
// in whatever an .HEIC is meant to open in.
func disambiguate(name, mediaKey string) string {
	extension := filepath.Ext(name)
	return strings.TrimSuffix(name, extension) + "_" + shortKey(mediaKey) + extension
}

func sameTarget(path, target string) bool {
	current, err := os.Readlink(path)
	return err == nil && current == target
}

// prune removes album directories that no longer correspond to a followed album — after an
// unfollow, or a rename upstream.
//
// It removes only symlinks and the directories that hold nothing else. A directory with a
// real file in it is left alone entirely: this tree is disposable, but a file someone put
// here by hand is not, and no convenience feature should be able to delete data.
func prune(root string, wanted map[string]bool, report *Report) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		switch {
		case !entry.IsDir():
			report.Skipped++
		case wanted[entry.Name()]:
			continue
		default:
			if err := removeLinkDir(filepath.Join(root, entry.Name()), report); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeLinkDir(directory string, report *Report) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}

	emptied := true
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 {
			emptied = false
			report.Skipped++
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			return err
		}
		report.Removed++
	}

	if !emptied {
		return nil
	}
	return os.Remove(directory)
}
