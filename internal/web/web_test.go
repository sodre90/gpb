package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpb/internal/auth"
	"gpb/internal/config"
	"gpb/internal/store"
	"gpb/internal/syncer"
)

const testPassword = "hunter2hunter2"

type recordedNotifications struct {
	mu     sync.Mutex
	events []string
}

func (r *recordedNotifications) record(event, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordedNotifications) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func testServer(t *testing.T) (*Server, *recordedNotifications) {
	t.Helper()

	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("loading a scratch config: %v", err)
	}
	// The default photos dir is /photos, which exists in the container and nowhere else. Tests
	// that serve a backed-up file, or measure the room left where the photos go, need a real one.
	cfg.PhotosDir = t.TempDir()

	if err := cfg.SetPassword(testPassword); err != nil {
		t.Fatalf("setting the test password: %v", err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening a scratch store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	manager := auth.NewManager(t.TempDir())
	notifications := &recordedNotifications{}

	server := NewServer(Deps{
		Config: cfg,
		Auth:   manager,
		Reauth: &fakeLoginBrowser{},
		Store:  db,
		Runs:   &fakeRuns{},
		Notify: notifications.record,
	})
	server.wait = func(time.Duration) {}
	// Never the real one: stopTheProcess signals whatever process it is running in, and here that
	// is the test binary.
	server.stop = func() error { return nil }
	return server, notifications
}

// fakeLoginBrowser stands in for the process stack: it remembers who started it and what
// stopped it, which is all the handlers can see of a real one.
type fakeLoginBrowser struct {
	mu       sync.Mutex
	running  bool
	owner    string
	stopping []string
}

func (f *fakeLoginBrowser) Start(sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.running {
		return auth.ErrReauthRunning
	}
	f.running, f.owner = true, sessionID
	return nil
}

func (f *fakeLoginBrowser) Stop(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.running {
		return
	}
	f.running, f.owner = false, ""
	f.stopping = append(f.stopping, reason)
}

func (f *fakeLoginBrowser) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeLoginBrowser) Owner() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		return ""
	}
	return f.owner
}

func (f *fakeLoginBrowser) OwnedBy(sessionID string) bool {
	return f.Owner() == sessionID && sessionID != ""
}

func (f *fakeLoginBrowser) StartedAt() time.Time   { return time.Time{} }
func (f *fakeLoginBrowser) RemoteCanvas() bool     { return true }
func (f *fakeLoginBrowser) WebsockifyAddr() string { return "127.0.0.1:0" }
func (f *fakeLoginBrowser) Available() error       { return nil }
func (f *fakeLoginBrowser) Touch()                 {}

func (f *fakeLoginBrowser) reasonsStopped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopping...)
}

func browserOf(server *Server) *fakeLoginBrowser { return server.reauth.(*fakeLoginBrowser) }

// fakeRuns records what the UI asked for without starting a browser.
type fakeRuns struct {
	mu       sync.Mutex
	started  []string
	activity string
	progress syncer.Progress
	err      error
}

func (f *fakeRuns) StartSync(reason string) error    { return f.record("sync", reason) }
func (f *fakeRuns) StartRefresh(reason string) error { return f.record("refresh", reason) }

func (f *fakeRuns) StartAlbumListing(albumID, reason string) error {
	return f.record("list:"+albumID, reason)
}

func (f *fakeRuns) record(kind, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}
	f.started = append(f.started, kind+":"+reason)
	return nil
}

func (f *fakeRuns) Activity() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activity
}

func (f *fakeRuns) Progress() syncer.Progress {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.progress
}

// nowRunning is what the daemon reports mid-job: what it is doing, and — for a sync, once it has
// opened its row — which run it is writing to.
func (f *fakeRuns) nowRunning(activity string, progress syncer.Progress) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activity, f.progress = activity, progress
}

func (f *fakeRuns) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

func runsOf(server *Server) *fakeRuns { return server.runs.(*fakeRuns) }

func postForm(handler http.Handler, path string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		request.AddCookie(cookie)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func get(handler http.Handler, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		request.AddCookie(cookie)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func sessionCookieFrom(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatal("the response set no session cookie")
	return nil
}

func login(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	recorder := postForm(handler, "/login", url.Values{"password": {testPassword}}, nil)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("login returned %d, want 303", recorder.Code)
	}
	return sessionCookieFrom(t, recorder)
}

func TestUnauthenticatedGETRedirectsToLogin(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	recorder := get(handler, "/", nil)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("GET / returned %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/login?next=/" {
		t.Errorf("redirected to %q, want /login?next=/", location)
	}
}

// A daemon restart empties the session store, and every open page immediately asks for its
// regions. Answering those with a redirect meant fetch() followed it without a word and the
// script swapped the login page into region after region, leaving a page stacked with login
// forms under whatever heading it had before.
func TestALiveFragmentWithNoSessionSaysSoRatherThanHandingBackTheLoginPage(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	for _, path := range []string{"/live/overview", "/live/nav", "/live/runs/now", "/live/banner"} {
		recorder := get(handler, path, nil)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no session returned %d, want 401", path, recorder.Code)
		}
		if strings.Contains(recorder.Body.String(), `name="password"`) {
			t.Errorf("GET %s answered with the login form, which the script would swap into the page", path)
		}
	}
}

func TestUnauthenticatedPOSTIsRejectedNotRedirected(t *testing.T) {
	server, _ := testServer(t)

	recorder := postForm(server.Handler(), "/warmup", nil, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("POST /warmup returned %d, want 401", recorder.Code)
	}
}

func TestLoginRejectsTheWrongPassword(t *testing.T) {
	server, _ := testServer(t)

	recorder := postForm(server.Handler(), "/login", url.Values{"password": {"wrong"}}, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong password returned %d, want 401", recorder.Code)
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" {
			t.Fatal("a failed login handed out a session cookie")
		}
	}
}

// The delay exists to slow guessing, and only a guess needs slowing. Charging it to the correct
// password too taxed the one person entitled to be here on every visit and bought nothing: the
// 303 and the 401 already tell an attacker which answer they got.
func TestOnlyAWrongPasswordIsMadeToWait(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	var charged []time.Duration
	server.wait = func(delay time.Duration) { charged = append(charged, delay) }

	login(t, handler)
	if len(charged) != 0 {
		t.Errorf("the right password was charged %v, and should have waited for nothing", charged)
	}

	if recorder := postForm(handler, "/login", url.Values{"password": {"wrong"}}, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong password returned %d, want 401", recorder.Code)
	}
	if len(charged) != 1 || charged[0] != loginFailureDelay {
		t.Errorf("a wrong password was charged %v, want one wait of %s", charged, loginFailureDelay)
	}
}

func TestSuccessfulLoginGrantsAHardenedCookie(t *testing.T) {
	server, _ := testServer(t)
	cookie := login(t, server.Handler())

	if !cookie.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("the session cookie is SameSite=%v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("the session cookie path is %q, want /", cookie.Path)
	}
}

func TestTheGooglePageShowsTheSessionAndItsDiagnostics(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	recorder := get(handler, "/reauth", login(t, handler))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /reauth returned %d, want 200", recorder.Code)
	}

	body := recorder.Body.String()
	for _, want := range []string{"Last confirmed", "Diagnostics", "Cookies held"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Google page is missing %q; body was:\n%s", want, body)
		}
	}
}

// /status was this page before it had a name, and the auth_required notification has been
// sending people to it for months.
func TestTheOldStatusPathStillLeadsSomewhere(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	recorder := get(handler, "/status", login(t, handler))
	if recorder.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /status returned %d, want 301", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/reauth" {
		t.Errorf("/status leads to %q, want /reauth", location)
	}
}

func TestLogoutInvalidatesTheSession(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	if recorder := postForm(handler, "/logout", nil, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("logout returned %d, want 303", recorder.Code)
	}
	if recorder := get(handler, "/", cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("the session still worked after logout: %d", recorder.Code)
	}
}

// `gpb passwd` writes config.toml from a separate process, so the running daemon has to
// notice without being restarted.
func TestPasswordChangesTakeEffectWithoutRestart(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	rotated, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reloading the config: %v", err)
	}
	if err := rotated.SetPassword("rotatedpassword"); err != nil {
		t.Fatalf("rotating the password: %v", err)
	}

	if recorder := postForm(handler, "/login", url.Values{"password": {testPassword}}, nil); recorder.Code != http.StatusUnauthorized {
		t.Errorf("the superseded password still logged in: %d", recorder.Code)
	}
	if recorder := postForm(handler, "/login", url.Values{"password": {"rotatedpassword"}}, nil); recorder.Code != http.StatusSeeOther {
		t.Errorf("the new password was not picked up: %d", recorder.Code)
	}
}

func TestLoginLocksOutAfterRepeatedFailures(t *testing.T) {
	server, notifications := testServer(t)
	handler := server.Handler()

	for attempt := 0; attempt < loginFailureLimit; attempt++ {
		if recorder := postForm(handler, "/login", url.Values{"password": {"wrong"}}, nil); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d returned %d, want 401", attempt, recorder.Code)
		}
	}

	recorder := postForm(handler, "/login", url.Values{"password": {testPassword}}, nil)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("the lockout did not engage: %d", recorder.Code)
	}

	events := notifications.all()
	if len(events) != 1 || events[0] != "web_lockout" {
		t.Errorf("lockout notifications were %v, want exactly one web_lockout", events)
	}
}

// The limit has to bound guesses rather than rounds of guessing. Fired together instead of one
// after another, unsynchronised attempts all read a clean counter and all get a full try against
// the hash — and each try in flight holds argon2's 64 MiB, which makes the same trick a way to
// run a box out of memory as well as a way to guess more than ten times.
func TestSimultaneousGuessesAreStillCountedOneAtATime(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	const fired = loginFailureLimit * 3
	answers := make(chan int, fired)
	var released sync.WaitGroup
	released.Add(1)
	for guess := 0; guess < fired; guess++ {
		go func() {
			released.Wait()
			answers <- postForm(handler, "/login", url.Values{"password": {"wrong"}}, nil).Code
		}()
	}
	released.Done()

	tried := 0
	for guess := 0; guess < fired; guess++ {
		if <-answers == http.StatusUnauthorized {
			tried++
		}
	}

	if tried > loginFailureLimit {
		t.Errorf("%d of %d simultaneous guesses were tried against the hash, where the limit is %d",
			tried, fired, loginFailureLimit)
	}
}

// No page may reload itself under the reader. A meta refresh loses scroll position, restarts
// every image and closes an open viewer, and it fired at exactly the moment someone was most
// likely to be looking — while a run worked.
func TestNoPageReloadsItselfUnderTheReader(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	runsOf(server).activity = "sync"

	for _, path := range []string{"/", "/albums", "/photos", "/runs", "/review", "/settings", "/reauth"} {
		if body := get(handler, path, cookie).Body.String(); strings.Contains(body, `http-equiv="refresh"`) {
			t.Errorf("%s reloads itself while a run is in flight", path)
		}
	}
}

// A live login flow must never auto-refresh: the reload would tear down the noVNC canvas
// under a user halfway through signing in.
func TestReauthDoesNotRefreshUnderALiveFlow(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	recorder := get(handler, "/reauth", login(t, handler))
	if strings.Contains(recorder.Body.String(), `http-equiv="refresh"`) {
		t.Error("the re-auth page auto-refreshes with no warmup running")
	}
}

func TestReauthSocketRefusesNonOwners(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	recorder := get(handler, "/reauth/ws", login(t, handler))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("the VNC bridge answered a session that owns no flow: %d", recorder.Code)
	}
}

// A running login browser holds a signed-in Google window, and only the session that started it
// could ever close it. Logging out with one open therefore stranded it: nothing in the UI could
// reach it again, and it stayed up until the idle timer noticed.
func TestLoggingOutClosesTheLoginBrowserItStarted(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)
	startLoginBrowser(t, handler, cookie)

	if recorder := postForm(handler, "/logout", nil, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("logout returned %d, want 303", recorder.Code)
	}

	browser := browserOf(server)
	if browser.Running() {
		t.Error("logging out left the login browser running with nobody able to close it")
	}
	if reasons := browser.reasonsStopped(); len(reasons) != 1 {
		t.Errorf("the browser was stopped %v, want exactly once", reasons)
	}
}

// A session can end without logging out — the idle window closes, or the tab is simply
// abandoned. The browser it started outlives it, so the next session has to be able to take it on.
func TestALoginBrowserNobodyOwnsAnyMoreIsTakenOverByTheNextSession(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	abandoned := login(t, handler)
	startLoginBrowser(t, handler, abandoned)
	server.sessions.destroy(abandoned.Value)

	inherited := login(t, handler)
	if body := get(handler, "/reauth", inherited).Body.String(); !strings.Contains(body, "/reauth/stop") {
		t.Errorf("the page offers the new session no way to close the browser; body was:\n%s", body)
	}
	if recorder := postForm(handler, "/reauth/stop", nil, inherited); recorder.Code != http.StatusSeeOther {
		t.Fatalf("the new session could not close the abandoned browser: %d", recorder.Code)
	}
	if browserOf(server).Running() {
		t.Error("the abandoned browser is still running")
	}
}

// Taking over an abandoned browser must not become a way to shut one somebody is signing in with.
func TestALoginBrowserInUseIsLeftAloneByOtherSessions(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	owner := login(t, handler)
	startLoginBrowser(t, handler, owner)

	bystander := login(t, handler)
	if recorder := postForm(handler, "/reauth/stop", nil, bystander); recorder.Code != http.StatusForbidden {
		t.Fatalf("another live session closed a browser it did not start: %d", recorder.Code)
	}
	if !browserOf(server).Running() {
		t.Error("the login browser was torn down under the session using it")
	}
}

func startLoginBrowser(t *testing.T, handler http.Handler, cookie *http.Cookie) {
	t.Helper()
	recorder := postForm(handler, "/reauth/start", url.Values{"password": {testPassword}}, cookie)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("starting the login browser returned %d, want 303", recorder.Code)
	}
}

func TestReauthStartNeedsThePasswordAgain(t *testing.T) {
	server, notifications := testServer(t)
	handler := server.Handler()

	recorder := postForm(handler, "/reauth/start", url.Values{"password": {"wrong"}}, login(t, handler))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("a stolen session opened the login browser without the password: %d", recorder.Code)
	}
	if events := notifications.all(); len(events) != 0 {
		t.Errorf("a single bad confirmation raised %v", events)
	}
}

// The re-auth form takes the same password as the login form and opens a browser signed into
// the Google account, so guessing at it has to cost what guessing at the login form costs. It
// counted failures without ever reading the count, which made it the unlimited door.
func TestGuessingAtTheReauthPasswordIsLockedOutToo(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	for attempt := 0; attempt < loginFailureLimit; attempt++ {
		if recorder := postForm(handler, "/reauth/start", url.Values{"password": {"wrong"}}, cookie); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d returned %d, want 401", attempt, recorder.Code)
		}
	}

	if recorder := postForm(handler, "/reauth/start", url.Values{"password": {"wrong"}}, cookie); recorder.Code != http.StatusTooManyRequests {
		t.Errorf("the re-auth form went on answering guesses: %d", recorder.Code)
	}
	if recorder := postForm(handler, "/login", url.Values{"password": {testPassword}}, nil); recorder.Code != http.StatusTooManyRequests {
		t.Errorf("failures charged to the re-auth form did not count against the login form: %d", recorder.Code)
	}
}

func TestSafeNextRejectsOffSiteTargets(t *testing.T) {
	cases := map[string]string{
		"/reauth":              "/reauth",
		"/album/1":             "/album/1",
		"":                     "/",
		"//evil.example":       "/",
		"https://evil.example": "/",
		"\\\\evil.example":     "/",
		"/\\evil.example":      "/",
	}

	for next, want := range cases {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

func testSessionStore(t *testing.T, idleTTL time.Duration) *sessionStore {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening a scratch store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return newSessionStore(db, idleTTL)
}

func TestSessionsExpireWhenIdle(t *testing.T) {
	sessions := testSessionStore(t, 20*time.Millisecond)

	id, err := sessions.create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !sessions.touch(id) {
		t.Fatal("a fresh session was rejected")
	}

	time.Sleep(40 * time.Millisecond)
	if sessions.touch(id) {
		t.Fatal("an idle session survived past its TTL")
	}
}

func TestTouchSlidesTheIdleWindow(t *testing.T) {
	sessions := testSessionStore(t, 60*time.Millisecond)
	id, _ := sessions.create()

	for range 4 {
		time.Sleep(20 * time.Millisecond)
		if !sessions.touch(id) {
			t.Fatal("an actively used session expired")
		}
	}
}

func TestUnknownSessionIDsAreRejected(t *testing.T) {
	sessions := testSessionStore(t, time.Hour)
	if sessions.touch("") || sessions.touch("made-up") {
		t.Fatal("the store accepted an id it never issued")
	}
}

// A redeploy is a routine Tuesday for an appliance, and it used to sign the one user out: the
// sessions lived in a map that went with the process. The cookie in the browser outlives it.
func TestASessionOutlivesTheProcessThatIssuedIt(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening a scratch store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	id, err := newSessionStore(db, time.Hour).create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	restarted := newSessionStore(db, time.Hour)
	if !restarted.touch(id) {
		t.Error("the session did not survive the daemon it was issued by")
	}
}

// The row is a bearer token: whoever holds the value in it is logged in. A database that carried
// the cookie itself would hand a live session to anyone who read the file — which sits next to
// the browser profile, so the file is already the thing worth protecting.
func TestTheDatabaseNeverHoldsTheCookieItself(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening a scratch store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	sessions := newSessionStore(db, time.Hour)
	id, err := sessions.create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	live, err := db.WebSessionIsLive(id, time.Now().Add(-time.Hour), time.Now().Add(-absoluteTTL))
	if err != nil {
		t.Fatalf("reading the session back: %v", err)
	}
	if live {
		t.Error("the session id itself is a key into the table, so it is stored as issued")
	}
	if !sessions.stillOpen(id) {
		t.Error("the session cannot be found by the id that issued it")
	}
}

// A session nobody comes back to has to leave the table on its own, or a store that only sweeps
// at a login keeps every dead session for as long as one live cookie keeps working.
func TestAnAbandonedSessionIsSweptWhenTheNextOneStarts(t *testing.T) {
	sessions := testSessionStore(t, 20*time.Millisecond)

	abandoned, err := sessions.create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	time.Sleep(40 * time.Millisecond)
	if _, err := sessions.create(); err != nil {
		t.Fatalf("create: %v", err)
	}

	held, err := sessions.db.WebSessionIsLive(fingerprint(abandoned), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("reading the abandoned session: %v", err)
	}
	if held {
		t.Error("the abandoned session is still in the table")
	}
}

func TestHealthzIsUnauthenticatedAndTerse(t *testing.T) {
	server, _ := testServer(t)

	recorder := get(server.Handler(), "/healthz", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /healthz returned %d, want 200", recorder.Code)
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != string(auth.StateUnknown) {
		t.Errorf("healthz said %q, want %q", got, auth.StateUnknown)
	}
}

// googleIn is a Google session in whatever state the test needs, which no test can drive a real
// browser into.
type googleIn struct{ state auth.State }

func (g googleIn) Status() auth.Status { return auth.Status{State: g.state} }

func (g googleIn) Warmup(context.Context) (*auth.Session, error) { return nil, nil }

// The two ways the Google half goes wrong need different words on the page, because they need
// different things from the user: one is "sign in again" and the other is anything but.
func TestTheBannerTellsASignedOutSessionFromACheckThatFailed(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	server.auth = googleIn{state: auth.StateAuthRequired}
	signedOut := get(handler, "/", cookie).Body.String()
	if !strings.Contains(signedOut, "signed this session out") {
		t.Errorf("a signed-out session says nothing about signing in; body was:\n%s", signedOut)
	}
	if !strings.Contains(signedOut, "banner danger") {
		t.Errorf("a signed-out session is not shown as serious; body was:\n%s", signedOut)
	}

	server.auth = googleIn{state: auth.StateWarmupFailed}
	couldNotCheck := get(handler, "/", cookie).Body.String()
	if strings.Contains(couldNotCheck, "signed this session out") {
		t.Errorf("a failed check claims Google signed the user out; body was:\n%s", couldNotCheck)
	}
	if !strings.Contains(couldNotCheck, "did not finish") {
		t.Errorf("a failed check is not reported at all; body was:\n%s", couldNotCheck)
	}

	server.auth = googleIn{state: auth.StateOK}
	if healthy := get(handler, "/", cookie).Body.String(); strings.Contains(healthy, "session-banner") {
		t.Errorf("a healthy session raised a banner; body was:\n%s", healthy)
	}
}
