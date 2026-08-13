package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

const (
	photosURL        = "https://photos.google.com/"
	warmupTimeout    = 90 * time.Second
	tokenWaitTimeout = 20 * time.Second

	browserCloseTimeout = 10 * time.Second
	signInHost          = "accounts.google.com"
	rejectedMarker      = "signin/rejected"

	// The warmup gets a display of its own rather than sharing the login browser's. The two
	// never run at once — the profile lock sees to that — but an Xvfb outliving either would
	// refuse the other its display number.
	warmupDisplay    = ":98"
	warmupWindowSize = "1280,900"
)

var ErrAuthRequired = errors.New("google session requires interactive re-authentication")

type State string

const (
	StateUnknown State = "unknown"
	StateOK      State = "ok"
	// StateAuthRequired means Google itself said no: the warmup landed on the sign-in page, or a
	// request came back refused. It asks the user for the one thing only they can do.
	StateAuthRequired State = "auth_required"
	// StateWarmupFailed means the check could not be made — no network, no browser, a timeout.
	// It is deliberately not auth_required: the session is probably fine, and asking someone to
	// sign in again over a DNS failure both wastes a login and makes the real thing
	// indistinguishable from noise the day it happens.
	StateWarmupFailed State = "warmup_failed"
)

type Status struct {
	State          State
	LastWarmup     time.Time
	LastSuccess    time.Time
	LastError      string
	CookieCount    int
	UserAgent      string
	UsingNoSandbox bool
	Warming        bool
}

type Manager struct {
	profileDir  string
	display     displayMode
	profileBusy atomic.Bool
	warming     atomic.Int32
	// lockFile is only ever touched by the goroutine that won profileBusy, so it needs no
	// lock of its own.
	lockFile *os.File

	mu          sync.Mutex
	session     *Session
	state       State
	lastWarmup  time.Time
	lastSuccess time.Time
	lastError   string
	noSandbox   bool
}

func NewManager(profileDir string) *Manager {
	return &Manager{profileDir: profileDir, display: detectDisplayMode(), state: StateUnknown}
}

// AcquireProfile claims exclusive use of the Chrome user data directory — both within this
// process and against any other gpb process sharing the data directory, which is what stops
// `gpb sync` from tearing the profile out from under a running daemon.
//
// Chrome has a SingletonLock of its own, but it fails late and messily. An advisory lock
// taken first turns "something else is already driving this profile" into a clean refusal.
func (m *Manager) AcquireProfile() bool {
	if !m.profileBusy.CompareAndSwap(false, true) {
		return false
	}
	if err := m.lockProfile(); err != nil {
		m.profileBusy.Store(false)
		return false
	}
	m.clearAbandonedSingleton()
	return true
}

// Chrome marks a profile in use with a symlink naming the host and pid that hold it, and
// decides a leftover is stale by checking that host against its own. In a container the host
// name is new on every start, so Chrome can never recognise its own leftovers as dead: one
// killed browser locks the profile permanently, and only a human deleting the file undoes it.
// Measured 2026-08-10 on the Fedora box, where it wedged the daemon into a restart loop.
//
// Removing the marker is safe exactly here and nowhere else, because the advisory lock taken
// above already answers — properly, and across processes — the question Chrome was guessing at.
func (m *Manager) clearAbandonedSingleton() {
	for _, marker := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
		os.Remove(filepath.Join(m.profileDir, marker))
	}
}

func (m *Manager) ReleaseProfile() {
	m.unlockProfile()
	m.profileBusy.Store(false)
}

// The lock lives beside the profile rather than inside it: Chrome rewrites the directory
// wholesale, and a lock file it does not know about should not be somewhere it may delete.
func (m *Manager) lockPath() string {
	return filepath.Clean(m.profileDir) + ".lock"
}

func (m *Manager) lockProfile() error {
	if err := os.MkdirAll(filepath.Dir(m.lockPath()), 0o700); err != nil {
		return err
	}

	file, err := os.OpenFile(m.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return err
	}

	m.lockFile = file
	return nil
}

// The lock is released by closing the file, which the kernel does even if this process dies,
// so a crashed sync cannot leave the profile permanently claimed.
func (m *Manager) unlockProfile() {
	if m.lockFile == nil {
		return
	}
	syscall.Flock(int(m.lockFile.Fd()), syscall.LOCK_UN)
	m.lockFile.Close()
	m.lockFile = nil
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	status := Status{
		State:          m.state,
		LastWarmup:     m.lastWarmup,
		LastSuccess:    m.lastSuccess,
		LastError:      m.lastError,
		UsingNoSandbox: m.noSandbox,
		Warming:        m.warming.Load() > 0,
	}
	if m.session != nil {
		status.CookieCount = m.session.CookieCount()
		status.UserAgent = m.session.UserAgent()
	}
	return status
}

func (m *Manager) Session() (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session, m.session != nil
}

// Warmup drives the persistent profile through a real page load so Google's own scripts
// rotate the __Secure-*PSIDTS cookies, then exports the refreshed credentials.
func (m *Manager) Warmup(ctx context.Context) (*Session, error) {
	m.warming.Add(1)
	defer m.warming.Add(-1)
	return m.runWarmup(ctx)
}

// WarmupInBackground reports the warmup as in flight before it returns, so a page rendered
// by the very next request already shows the check rather than the verdict it supersedes.
// Callers that answer an HTTP request cannot wait for a warmup: it can take a minute.
func (m *Manager) WarmupInBackground(ctx context.Context, done func(*Session, error)) {
	m.warming.Add(1)
	go func() {
		defer m.warming.Add(-1)
		done(m.runWarmup(ctx))
	}()
}

func (m *Manager) runWarmup(ctx context.Context) (*Session, error) {
	if !m.AcquireProfile() {
		return nil, ErrProfileBusy
	}
	defer m.ReleaseProfile()

	session, err := m.attemptWarmup(ctx, m.usingNoSandbox())

	if err != nil && !errors.Is(err, ErrAuthRequired) && ctx.Err() == nil && !m.usingNoSandbox() {
		log.Printf("auth: warmup failed (%v); retrying with --no-sandbox", err)
		m.setNoSandbox()
		session, err = m.attemptWarmup(ctx, true)
	}

	// A warmup the caller abandoned — shutdown, or a sync run cancelled from the UI — is no
	// evidence about the session. Recording it would blame Google for our own cancellation,
	// flip the health state and page the user on the way out the door. The inner
	// warmupTimeout is deliberately not covered: that one is a genuine hang.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	m.record(session, err)
	return session, err
}

func (m *Manager) attemptWarmup(ctx context.Context, noSandbox bool) (*Session, error) {
	ctx, cancel := context.WithTimeout(ctx, warmupTimeout)
	defer cancel()

	display, stopDisplay, err := m.displayForWarmup()
	if err != nil {
		return nil, err
	}
	defer stopDisplay()

	allocatorCtx, cancelAllocator := chromedp.NewExecAllocator(ctx, browserOptions(m.profileDir, display, noSandbox)...)
	defer cancelAllocator()

	browserCtx, cancelBrowser := chromedp.NewContext(allocatorCtx)
	defer cancelBrowser()
	defer closeBrowserCleanly(browserCtx)

	trail := followTheDocuments(browserCtx)

	var (
		location  string
		userAgent string
		wiz       map[string]any
	)
	err = chromedp.Run(browserCtx,
		// A warmup that reads photos.google.com out of the browser cache proves nothing about
		// the session. The login browser leaves a fresh copy of the shell behind — token and all
		// — so a warmup moments later can report a live session Google was never asked to
		// confirm, and the next one, past the cache, finds the account signed out. Measured
		// 2026-08-10 on the Fedora box: the warmup that "succeeded" three seconds after each
		// login rotated no cookie at all, which no real visit to Google leaves behind.
		network.SetCacheDisabled(true),
		chromedp.Navigate(photosURL),
		chromedp.WaitReady("body"),
		awaitSessionToken(),
		chromedp.Location(&location),
		chromedp.Evaluate("navigator.userAgent", &userAgent),
		chromedp.Evaluate("window.WIZ_global_data || {}", &wiz),
	)
	if err != nil {
		return nil, fmt.Errorf("browser warmup: %w", err)
	}

	cookies, err := readCookies(browserCtx)
	if err != nil {
		return nil, err
	}
	logWarmupObservation(location, wiz, cookies, trail)

	if !LoggedIn(location) {
		return nil, fmt.Errorf("%w: landed on %s", ErrAuthRequired, redactURL(location))
	}

	session, err := newSession(cookies, wiz, userAgent, time.Now())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthRequired, err)
	}
	return session, nil
}

// closeBrowserCleanly asks Chrome to shut itself down instead of letting chromedp's cancel
// SIGKILL it, and waits for it to go.
//
// This is not tidiness. Google rotates __Secure-1PSIDTS and __Secure-3PSIDTS during a visit,
// and without a current pair it treats the surviving session cookies as insufficient and
// serves the signed-out page. Chrome batches cookie writes in memory and commits them on a
// timer or at shutdown, and a warmup is over long before that timer, so a killed browser
// persists nothing it learned — measured 2026-08-10 on the Fedora box, where the cookie
// database had not been written since the last browser that was allowed to exit, and a
// sign-in that had just succeeded was gone by the next request.
//
// The shutdown is asked for with the CDP command rather than chromedp.Cancel, which expects
// to be the only thing tearing the context down and deadlocks against the cancels below.
//
// A browser that never started has nothing to close, and asking anyway is not merely futile:
// chromedp reads a missing browser as "not allocated yet" and starts a second one, whose
// teardown then closes an already-closed channel and takes the daemon down with it.
// The close runs on a context of its own rather than on the browser's. A warmup that ended
// because it was cancelled — shutdown, or a run stopped from the UI — is the case where the
// cookie flush matters most and the only one where the browser's context is already dead: a
// shutdown asked for on a dead context is refused before it is sent, and Chrome is killed
// holding everything it learned.
func closeBrowserCleanly(browserCtx context.Context) {
	if allocated := chromedp.FromContext(browserCtx); allocated == nil || allocated.Browser == nil {
		return
	}

	ctx, cancel := closeContext(browserCtx)
	defer cancel()

	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return browser.Close().Do(ctx)
	}))
	if err != nil {
		log.Printf("auth: browser did not shut down cleanly (%v); the google session may not survive it", err)
	}
}

func closeContext(browserCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(browserCtx), browserCloseTimeout)
}

// awaitSessionToken gives the page shell time to define WIZ_global_data. A body that is
// merely ready proves nothing: reading the token in the same tick as DOM readiness makes a
// live session look expired. A timeout here is not an error — the caller reports the
// missing token with far better context than a polling failure would.
func awaitSessionToken() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		chromedp.Poll(
			"!!(window.WIZ_global_data && window.WIZ_global_data."+wizTokenField+")",
			nil,
			chromedp.WithPollingTimeout(tokenWaitTimeout),
		).Do(ctx)
		return nil
	})
}

// logWarmupObservation records why a warmup reached its verdict. Names and counts only:
// cookie values and the token itself must never reach a log line.
func logWarmupObservation(location string, wiz map[string]any, cookies []*network.Cookie, trail *documentTrail) {
	_, hasToken := wizTokenFrom(wiz)
	log.Printf("auth: warmup observed %s — %d cookies, account cookie %v, token %v",
		redactURL(location), len(cookies), hasAccountCookie(cookies), hasToken)
	log.Printf("auth: google routed the warmup %s", trail)
}

// documentTrail is the sequence of top-level documents Google served on the way to wherever
// the warmup ended up. The final URL alone discards the reason for it: a 302 away from
// photos.google.com and a 200 on it that then hands off in script land in the same place but
// mean different things, and only the first says the session was refused at the edge.
type documentTrail struct {
	mu    sync.Mutex
	steps []string
}

func followTheDocuments(browserCtx context.Context) *documentTrail {
	trail := &documentTrail{}
	chromedp.ListenTarget(browserCtx, func(event any) {
		switch event := event.(type) {
		case *network.EventRequestWillBeSent:
			if event.RedirectResponse != nil {
				trail.record(event.RedirectResponse)
			}
		case *network.EventResponseReceived:
			if event.Type == network.ResourceTypeDocument {
				trail.record(event.Response)
			}
		}
	})
	return trail
}

func (t *documentTrail) record(response *network.Response) {
	step := fmt.Sprintf("%d %s", int(response.Status), redactURL(response.URL))
	if response.FromDiskCache {
		step += " (from cache)"
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.steps = append(t.steps, step)
}

func (t *documentTrail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.steps) == 0 {
		return "nowhere — no document response was seen"
	}
	return "through " + strings.Join(t.steps, " → ")
}

func readCookies(ctx context.Context) ([]*network.Cookie, error) {
	var cookies []*network.Cookie
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		fetched, err := storage.GetCookies().Do(ctx)
		if err != nil {
			return err
		}
		cookies = fetched
		return nil
	}))
	if err != nil {
		return nil, fmt.Errorf("reading cookies: %w", err)
	}
	return cookies, nil
}

// LoggedIn reports whether a post-navigation URL indicates a live session rather than a
// bounce to the sign-in flow.
func LoggedIn(location string) bool {
	if location == "" {
		return false
	}
	return !strings.Contains(location, signInHost) && !strings.Contains(location, rejectedMarker)
}

// Rejected reports whether Google refused the login as automated, which needs a different
// remedy from an ordinary expiry.
func Rejected(location string) bool {
	return strings.Contains(location, rejectedMarker)
}

func (m *Manager) record(session *Session, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastWarmup = time.Now()
	switch {
	case err == nil:
		m.session = session
		m.state = StateOK
		m.lastSuccess = m.lastWarmup
		m.lastError = ""
	case errors.Is(err, ErrAuthRequired):
		m.session = nil
		m.state = StateAuthRequired
		m.lastError = err.Error()
	default:
		// The session is left in place. A check that could not be made is not evidence against
		// the cookies it never got to use, and the run that follows may well work.
		m.state = StateWarmupFailed
		m.lastError = err.Error()
	}
}

// displayForWarmup gives the warmup browser a screen to run headful on, so that it announces
// itself to Google exactly as the browser the user signed in with did — headless Chrome says
// "HeadlessChrome/151.0.0.0" where the login browser says "Chrome/151.0.0.0", and overriding
// the string cannot fix that: every mechanism that changes it also empties Sec-CH-UA, trading
// one mismatch for another.
//
// This was adopted on 2026-08-10 believing the mismatch was why every session died minutes
// after sign-in. It was not: the container was running Chromium rather than Google Chrome, and
// swapping the binary is what fixed it. Whether a headless warmup would now hold a session is
// untested, so this stays until it is measured — one Xvfb per warmup is a cheap thing to be
// wrong about, and being wrong the other way costs a sign-in.
//
// A machine with a screen of its own is left headless: there, running the warmup headful
// would throw a browser window in the user's face twice a day, and the deployment this
// protects has no screen anyway.
func (m *Manager) displayForWarmup() (display string, stop func(), err error) {
	if m.display == hostDisplay {
		return "", func() {}, nil
	}
	stop, err = startVirtualDisplay(warmupDisplay)
	if err != nil {
		return "", func() {}, err
	}
	return warmupDisplay, stop, nil
}

func (m *Manager) usingNoSandbox() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.noSandbox
}

func (m *Manager) setNoSandbox() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.noSandbox = true
}

// browserOptions describes the warmup browser. It deliberately does not build on chromedp's
// defaults: those are Puppeteer's, meant for scraping a page and leaving, and the first of them
// — --disable-background-networking — switches off the very machinery this browser exists to
// run. Keeping a Google session alive is background work: Chrome re-checks it against
// accounts.google.com and takes a reissued __Secure-1PSIDTS in return.
//
// Measured 2026-08-10 on the Fedora box: on chromedp's defaults the warmup loaded
// photos.google.com signed in, took a 401 on its account-consistency check, wrote not a single
// cookie, and left the account signed out of every Google property behind it.
//
// What remains mirrors the browser the user signs in with, flag for flag, so the two are one
// browser as far as Google can tell. The allocator adds --remote-debugging-port itself.
func browserOptions(profileDir, display string, noSandbox bool) []chromedp.ExecAllocatorOption {
	options := []chromedp.ExecAllocatorOption{
		chromedp.UserDataDir(profileDir),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
	}

	if display == "" {
		options = append(options, chromedp.Flag("headless", "new"))
	} else {
		options = append(options,
			chromedp.Flag("headless", false),
			chromedp.Flag("window-size", warmupWindowSize),
			chromedp.Env("DISPLAY="+display))
	}
	if noSandbox {
		options = append(options, chromedp.NoSandbox)
	}
	if path := os.Getenv("GPB_CHROME_PATH"); path != "" {
		options = append(options, chromedp.ExecPath(path))
	}
	return options
}

func redactURL(raw string) string {
	if index := strings.IndexByte(raw, '?'); index >= 0 {
		return raw[:index] + "?<redacted>"
	}
	return raw
}
