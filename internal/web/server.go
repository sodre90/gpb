package web

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"gpb/internal/auth"
	"gpb/internal/config"
	"gpb/internal/homeassistant"
	"gpb/internal/store"
	"gpb/internal/syncer"
	"gpb/internal/thumbs"
)

const defaultNoVNCDir = "/usr/share/novnc"

// Notifier is the outbound alert hook. It is declared here because this is where it is
// consumed; the daemon supplies the implementation.
type Notifier func(event, detail string)

// Runs is the sync engine as the UI needs it: start work, say what is already running, and say
// how far it has got. Declaring it here rather than importing the daemon's concrete runner keeps
// the dependency pointing one way — the daemon builds the web server, never the reverse.
type Runs interface {
	StartSync(reason string) error
	StartRefresh(reason string) error
	StartAlbumListing(albumID, reason string) error
	Activity() string
	Progress() syncer.Progress
}

// MQTTBridge is the Home Assistant bridge as the settings page needs it: something that can say
// how it is getting on with the broker, and be told to try again with whatever was just saved.
type MQTTBridge interface {
	Status() homeassistant.Status
	Reload()
}

// GoogleSession is the browser-held login as the pages need it: what state it is in, and a way
// to prod it. Declared here for the same reason Runs is — and it is what lets the pages be
// photographed, and one day tested, in a state no test can drive a real browser into.
type GoogleSession interface {
	Status() auth.Status
	Warmup(ctx context.Context) (*auth.Session, error)
}

// LoginBrowser is the headful Google login the reauth page drives. An interface for the same
// reason GoogleSession is: no test can spawn a real browser, and whether one is running — and
// whose it is — decides what every one of those handlers is allowed to do.
type LoginBrowser interface {
	Start(sessionID string) error
	Stop(reason string)
	Running() bool
	OwnedBy(sessionID string) bool
	Owner() string
	StartedAt() time.Time
	RemoteCanvas() bool
	WebsockifyAddr() string
	Available() error
	Touch()
}

// Deps are what a server needs to exist. A struct rather than six positional arguments:
// they are all pointers and interfaces, and a call site that reads NewServer(a, b, c, d, e, f)
// tells a reviewer nothing about which is which.
type Deps struct {
	Config        config.Config
	Auth          *auth.Manager
	Reauth        LoginBrowser
	Store         *store.Store
	Runs          Runs
	Notify        Notifier
	HomeAssistant MQTTBridge
}

type Server struct {
	cfg      config.Config
	auth     GoogleSession
	reauth   LoginBrowser
	store    *store.Store
	runs     Runs
	sessions *sessionStore
	guard    *loginGuard
	notify   Notifier
	mqtt     MQTTBridge
	novncDir string
	live     *hub
	thumbs   *thumbs.Cache
	// images is an interface rather than the concrete *thumbSource so a test can exercise the
	// whole serving path — store row, cache, headers — without a Google session.
	images thumbs.Fetcher
	// places turns a name into a box on the map; nil when no lookup is configured, and then
	// the Photos page offers no search.
	places PlaceFinder

	// wait is time.Sleep under another name, so a test can watch the delay being charged rather
	// than time it against a clock that also counts the password hashing it cannot control.
	wait func(time.Duration)

	// stop is stopTheProcess under another name. A test that exercised the real one would signal
	// the test binary and take the whole run down with it.
	stop func() error

	// freeBytes is syncer.FreeBytes under another name, so the invented library the README is
	// photographed against can invent its free space too. Measuring the real disk put the host's
	// spare terabytes into a picture of a made-up backup, and changed the file every time.
	freeBytes func(dir string) (uint64, error)
}

func NewServer(deps Deps) *Server {
	if deps.Notify == nil {
		deps.Notify = func(string, string) {}
	}
	return &Server{
		cfg:       deps.Config,
		auth:      deps.Auth,
		reauth:    deps.Reauth,
		store:     deps.Store,
		runs:      deps.Runs,
		sessions:  newSessionStore(deps.Store, deps.Config.Web.SessionIdleTTL.Duration),
		guard:     &loginGuard{},
		notify:    deps.Notify,
		mqtt:      deps.HomeAssistant,
		novncDir:  noVNCDir(),
		live:      newHub(),
		thumbs:    thumbs.NewCache(deps.Config.ThumbCacheDir(), deps.Config.Thumbs.CacheMaxBytes),
		images:    newThumbSource(deps.Auth, deps.Config.Thumbs.RequestsPerSecond),
		places:    placeFinderFor(deps.Config.Places.LookupURL),
		wait:      time.Sleep,
		stop:      stopTheProcess,
		freeBytes: syncer.FreeBytes,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /static/", http.FileServerFS(assets))

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.Handle("POST /logout", s.requireSession(http.HandlerFunc(s.handleLogout)))

	mux.Handle("GET /{$}", s.requireSession(http.HandlerFunc(s.handleOverview)))
	mux.Handle("GET /albums", s.requireSession(http.HandlerFunc(s.handleAlbums)))
	mux.Handle("GET /photos", s.requireSession(http.HandlerFunc(s.handlePhotos)))
	mux.Handle("GET /photos/cells", s.requireSessionForFragment(http.HandlerFunc(s.handlePhotoCells)))
	mux.Handle("POST /album/{id}/mode", s.requireSession(http.HandlerFunc(s.handleAlbumMode)))
	mux.Handle("POST /album/{id}/favourite", s.requireSession(http.HandlerFunc(s.handleAlbumFavourite)))
	mux.Handle("POST /albums/refresh", s.requireSession(http.HandlerFunc(s.handleAlbumRefresh)))
	mux.Handle("POST /library", s.requireSession(http.HandlerFunc(s.handleLibrary)))
	mux.Handle("POST /sync", s.requireSession(http.HandlerFunc(s.handleSyncNow)))

	mux.Handle("GET /runs", s.requireSession(http.HandlerFunc(s.handleRuns)))

	mux.Handle("GET /ws", s.requireSessionForFragment(http.HandlerFunc(s.handleLiveSocket)))
	mux.Handle("GET /live/nav", s.requireSessionForFragment(http.HandlerFunc(s.handleLiveNav)))
	mux.Handle("GET /live/banner", s.requireSessionForFragment(http.HandlerFunc(s.handleLiveSessionBanner)))
	mux.Handle("GET /live/overview", s.requireSessionForFragment(http.HandlerFunc(s.handleLiveOverview)))
	mux.Handle("GET /live/runs/now", s.requireSessionForFragment(http.HandlerFunc(s.handleLiveRunsNow)))
	mux.Handle("GET /live/runs/history", s.requireSessionForFragment(http.HandlerFunc(s.handleLiveRunsHistory)))
	mux.Handle("GET /live/albums/now", s.requireSessionForFragment(http.HandlerFunc(s.handleLiveAlbumsNow)))
	mux.Handle("GET /live/album/{id}/listing", s.requireSessionForFragment(http.HandlerFunc(s.handleLiveAlbumListing)))
	mux.Handle("GET /live/reauth/status", s.requireSessionForFragment(http.HandlerFunc(s.handleLiveReauthStatus)))
	mux.Handle("GET /live/reauth/actions", s.requireSessionForFragment(http.HandlerFunc(s.handleLiveReauthActions)))

	mux.Handle("GET /review", s.requireSession(http.HandlerFunc(s.handleReview)))
	mux.Handle("POST /review/resolve", s.requireSession(http.HandlerFunc(s.handleReviewResolve)))
	mux.Handle("POST /review/copies", s.requireSession(http.HandlerFunc(s.handleReviewRemoveCopies)))

	mux.Handle("GET /settings", s.requireSession(http.HandlerFunc(s.handleSettings)))
	mux.Handle("POST /settings", s.requireSession(http.HandlerFunc(s.handleSettingsSave)))
	mux.Handle("POST /settings/broker", s.requireSession(http.HandlerFunc(s.handleBrokerRedial)))
	mux.Handle("POST /settings/restart", s.requireSession(http.HandlerFunc(s.handleRestart)))

	mux.Handle("GET /album/{id}", s.requireSession(http.HandlerFunc(s.handleAlbum)))
	mux.Handle("GET /album/{id}/cells", s.requireSessionForFragment(http.HandlerFunc(s.handleAlbumCells)))
	mux.Handle("POST /album/{id}/grid-mode", s.requireSession(http.HandlerFunc(s.handleAlbumModeFromGrid)))
	mux.Handle("POST /album/{id}/picks", s.requireSession(http.HandlerFunc(s.handlePicks)))
	mux.Handle("POST /album/{id}/refresh", s.requireSession(http.HandlerFunc(s.handleAlbumItemRefresh)))
	mux.Handle("GET /thumb/{key}", s.requireSession(http.HandlerFunc(s.handleThumb)))
	mux.Handle("GET /image/{key}", s.requireSession(http.HandlerFunc(s.handleImage)))
	mux.Handle("GET /album/{id}/cover", s.requireSession(http.HandlerFunc(s.handleAlbumCover)))

	// /status was the Google page before it had a name. Notifications and bookmarks still point
	// at it, so it keeps answering — by sending the reader to where its contents now live.
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/reauth", http.StatusMovedPermanently)
	})
	mux.Handle("POST /warmup", s.requireSession(http.HandlerFunc(s.handleWarmup)))

	mux.Handle("GET /reauth", s.requireSession(http.HandlerFunc(s.handleReauthPage)))
	mux.Handle("POST /reauth/start", s.requireSession(http.HandlerFunc(s.handleReauthStart)))
	mux.Handle("POST /reauth/stop", s.requireSession(http.HandlerFunc(s.handleReauthStop)))
	mux.Handle("GET /reauth/ws", s.requireSession(http.HandlerFunc(s.handleReauthSocket)))
	mux.Handle("GET /novnc/", s.requireSession(http.StripPrefix("/novnc/", http.FileServer(http.Dir(s.novncDir)))))

	protection := http.NewCrossOriginProtection()
	return protection.Handler(mux)
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.cfg.Web.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go s.watchForChanges(ctx)

	go func() {
		<-ctx.Done()
		s.live.closeAll()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	if err := s.serve(server); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// serve refuses to start rather than falling back to plain http when the certificate cannot be
// had. Quietly serving the thing the user turned off would be the one failure they would never
// notice, and the settings page has already tried the certificate once, when it was saved — so
// the only way to reach this is a file that has moved since.
func (s *Server) serve(server *http.Server) error {
	if !s.cfg.Web.TLS.Enabled() {
		return server.ListenAndServe()
	}

	certFile, keyFile, err := certificateFor(s.cfg, time.Now())
	if err != nil {
		return err
	}
	log.Printf("web: serving https with the certificate at %s", certFile)
	return server.ListenAndServeTLS(certFile, keyFile)
}

type contextKey string

const sessionContextKey contextKey = "gpb.session"

func (s *Server) requireSession(next http.Handler) http.Handler {
	return s.guardedBySession(next, s.rejectUnauthenticated)
}

// requireSessionForFragment guards what a page fetches for itself rather than navigates to. A
// redirect there is followed by fetch() without anyone noticing, and the login page it lands on
// is swapped into the region — which after a restart turned every region on an open page into a
// login form stacked one above the next. A status the script can read sends the reader to the
// login page as a page.
func (s *Server) requireSessionForFragment(next http.Handler) http.Handler {
	return s.guardedBySession(next, func(w http.ResponseWriter, r *http.Request) {
		s.clearSessionCookie(w)
		http.Error(w, "not authenticated", http.StatusUnauthorized)
	})
}

func (s *Server) guardedBySession(next http.Handler, reject http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !s.sessions.touch(cookie.Value) {
			reject(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey, cookie.Value)))
	})
}

func (s *Server) rejectUnauthenticated(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	if r.Method != http.MethodGet {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login?next="+r.URL.EscapedPath(), http.StatusSeeOther)
}

func sessionIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(sessionContextKey).(string)
	return id
}

// passwordHash re-reads the config file so `gpb passwd` takes effect without restarting
// the daemon. It is consulted only on the two password prompts, weeks apart.
func (s *Server) passwordHash() string {
	cfg, err := config.Load(s.cfg.DataDir())
	if err != nil {
		log.Printf("web: re-reading the config for the password hash: %v", err)
		return s.cfg.Web.PasswordHash
	}
	return cfg.Web.PasswordHash
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(string(s.auth.Status().State) + "\n"))
}

func noVNCDir() string {
	if dir := os.Getenv("GPB_NOVNC_DIR"); dir != "" {
		return dir
	}
	return defaultNoVNCDir
}
