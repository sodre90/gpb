package web

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// imageCacheSeconds is how long a browser may keep a full-size image. A backed-up file never
// changes once written — a new version from Google would be a new media key — so the only
// reason to ask twice is an eviction. Private, like every other pixel this server hands out.
const imageCacheSeconds = 30 * 24 * 60 * 60

// handleImage serves the file this backup actually holds, rather than Google's copy of it. The
// viewer wants a real photograph and the grid's 256-pixel thumbnail is not one; going to the
// disk gives full resolution, costs Google nothing, and keeps working when the session is dead,
// which is precisely when a person most wants to check what they have.
//
// Only downloaded items have a file. For everything else the viewer falls back to the
// thumbnail, so this answers 404 rather than inventing something.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.Item(r.PathValue("key"))
	if err != nil || item.LocalPath == "" {
		http.NotFound(w, r)
		return
	}

	file, info, err := s.openInPool(item.LocalPath)
	if err != nil {
		// A row saying "done" whose file is missing is worth knowing about: the next run will
		// notice and re-fetch it, but a log line is what connects a blank viewer to the cause.
		log.Printf("web: serving %s: %v", item.MediaKey, err)
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(imageCacheSeconds))
	http.ServeContent(w, r, filepath.Base(item.LocalPath), info.ModTime(), file)
}

// openInPool opens a stored path under the pool and nowhere else. The path comes from the
// database, which makes it as trustworthy as the database is; os.Root turns that question into
// one the kernel answers, so a row saying "../../etc/passwd" opens nothing rather than
// something. Paths are stored absolute, so they are made relative to the pool first.
func (s *Server) openInPool(localPath string) (*os.File, os.FileInfo, error) {
	pool, err := os.OpenRoot(s.cfg.PoolDir())
	if err != nil {
		return nil, nil, err
	}
	defer pool.Close()

	inside, err := filepath.Rel(s.cfg.PoolDir(), localPath)
	if err != nil {
		return nil, nil, err
	}
	if strings.HasPrefix(inside, "..") {
		return nil, nil, os.ErrNotExist
	}

	file, err := pool.Open(inside)
	if err != nil {
		return nil, nil, err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, info, nil
}
