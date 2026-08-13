package web

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"

	"golang.org/x/time/rate"

	"gpb/internal/auth"
	"gpb/internal/gphotos"
)

// thumbCacheSeconds is how long a browser may keep a thumbnail without asking again. A
// thumbnail is immutable for a media key, so the only reason to re-request one is that the
// browser evicted it. Marked private: this is one user's photo library.
const thumbCacheSeconds = 30 * 24 * 60 * 60

// thumbBurst lets a freshly opened grid page fill visibly faster than the steady rate, then
// settle back to it. The steady rate is what Google sees over a minute of scrolling.
const thumbBurst = 16

// thumbSource hands out a gphotos client built from whatever session the auth manager
// currently holds, rebuilding it when a warmup replaces the session. It exists because the
// grid needs Google at request time, while the sync engine builds its client per run.
type thumbSource struct {
	auth    *auth.Manager
	limiter *rate.Limiter

	mu      sync.Mutex
	session *auth.Session
	cached  *gphotos.Client
}

func newThumbSource(manager *auth.Manager, requestsPerSecond float64) *thumbSource {
	return &thumbSource{
		auth:    manager,
		limiter: rate.NewLimiter(rate.Limit(requestsPerSecond), thumbBurst),
	}
}

// clientForSession returns a client built from the session the manager holds right now.
// Thumbnails deliberately never trigger a warmup: a warmup takes a browser and a minute, and
// a grid page that quietly launched Chromium because someone scrolled would be a surprising
// thing for this app to do.
func (t *thumbSource) clientForSession() (*gphotos.Client, error) {
	session, ok := t.auth.Session()
	if !ok {
		return nil, auth.ErrAuthRequired
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.cached != nil && t.session == session {
		return t.cached, nil
	}

	client, err := gphotos.NewClient(session)
	if err != nil {
		return nil, err
	}
	// Its own bucket, not the sync engine's: this traffic exists only while a human is
	// scrolling, and throttling it to the nightly backup's pace would make the grid unusable.
	client.Throttle(t.limiter)

	t.session, t.cached = session, client
	return client, nil
}

func (t *thumbSource) Thumbnail(ctx context.Context, baseURL string, w io.Writer) (int64, error) {
	client, err := t.clientForSession()
	if err != nil {
		return 0, err
	}
	return client.Thumbnail(ctx, baseURL, w)
}

// handleThumb serves one grid image. Every failure produces a placeholder rather than a
// broken-image icon: a grid of 200 cells will always have some item Google is unhappy about,
// and the page has to stay usable for picking the other 199.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	mediaKey := r.PathValue("key")

	item, err := s.store.Item(mediaKey)
	if err != nil {
		s.servePlaceholder(w, r, http.StatusNotFound)
		return
	}

	image, err := s.thumbs.Get(r.Context(), item.MediaKey, item.ThumbnailURL, s.images)
	if err != nil {
		if !errors.Is(err, gphotos.ErrNoThumbnail) {
			log.Printf("web: fetching a thumbnail: %v", err)
		}
		s.servePlaceholder(w, r, http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(thumbCacheSeconds))
	w.Header().Set("Content-Length", strconv.Itoa(len(image)))
	w.Write(image)
}

// handleAlbumCover serves the picture Google puts on an album in its own UI. The album list
// needs it for the same reason Google does: forty-nine of this library's rows are bundles of
// shared photos with no name, and a cover is the only thing that tells them apart at a glance.
func (s *Server) handleAlbumCover(w http.ResponseWriter, r *http.Request) {
	album, err := s.store.Album(r.PathValue("id"))
	if err != nil || album.CoverURL == "" {
		s.servePlaceholder(w, r, http.StatusNotFound)
		return
	}

	image, err := s.thumbs.Get(r.Context(), album.ID, album.CoverURL, s.images)
	if err != nil {
		if !errors.Is(err, gphotos.ErrNoThumbnail) {
			log.Printf("web: fetching an album cover: %v", err)
		}
		s.servePlaceholder(w, r, http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	// Covers get a shorter life than grid thumbnails: unlike a media key's image, an album's
	// cover changes when its contents do.
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(coverCacheSeconds))
	w.Header().Set("Content-Length", strconv.Itoa(len(image)))
	w.Write(image)
}

const coverCacheSeconds = 24 * 60 * 60

// servePlaceholder answers with a grey square. It carries no cache headers: the next page
// view should try Google again, because the usual cause is a session that will come back.
func (s *Server) servePlaceholder(w http.ResponseWriter, r *http.Request, status int) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(placeholderSVG)
}

// placeholderSVG picks its own greys from the browser's colour scheme. It used to be a fixed
// dark square, which in a light page was a black hole where a photo should be. An <img> loads
// this as a document of its own, so it cannot inherit the page's colours — it has to ask.
var placeholderSVG = []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">` +
	`<style>:root{--face:#e9e9e7;--mark:#b8bcc4}` +
	`@media (prefers-color-scheme: dark){:root{--face:#2a2d33;--mark:#4a4e57}}</style>` +
	`<rect width="64" height="64" fill="var(--face)"/>` +
	`<path d="M20 40l8-10 6 7 5-5 5 8z" fill="var(--mark)"/>` +
	`<circle cx="24" cy="24" r="4" fill="var(--mark)"/></svg>`)
