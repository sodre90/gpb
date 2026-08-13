package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
)

func TestLoggedInReadsLandingURL(t *testing.T) {
	cases := map[string]bool{
		"https://photos.google.com/":       true,
		"https://photos.google.com/albums": true,
		"":                                 false,
		"https://accounts.google.com/ServiceLogin?continue=x":        false,
		"https://accounts.google.com/v3/signin/identifier":           false,
		"https://accounts.google.com/signin/rejected?rrk=48&dsh=123": false,
	}

	for location, want := range cases {
		if got := LoggedIn(location); got != want {
			t.Errorf("LoggedIn(%q) = %v, want %v", location, got, want)
		}
	}
}

func TestRejectedDistinguishesAutomationBlock(t *testing.T) {
	if !Rejected("https://accounts.google.com/signin/rejected?rrk=48") {
		t.Error("an automation block was not recognised")
	}
	if Rejected("https://accounts.google.com/v3/signin/identifier") {
		t.Error("an ordinary sign-in page was reported as an automation block")
	}
}

func TestRedactURLDropsQueryStrings(t *testing.T) {
	got := redactURL("https://accounts.google.com/signin/rejected?rrk=48&dsh=secret")
	if got != "https://accounts.google.com/signin/rejected?<redacted>" {
		t.Errorf("redactURL leaked the query string: %q", got)
	}
	if got := redactURL("https://photos.google.com/"); got != "https://photos.google.com/" {
		t.Errorf("redactURL mangled a query-free URL: %q", got)
	}
}

func TestNewSessionRequiresToken(t *testing.T) {
	if _, err := newSession(nil, map[string]any{}, "UA", time.Now()); err != ErrNoToken {
		t.Errorf("a page shell without SNlM0e gave err=%v, want ErrNoToken", err)
	}
	if _, err := newSession(nil, map[string]any{wizTokenField: ""}, "UA", time.Now()); err != ErrNoToken {
		t.Errorf("an empty SNlM0e was accepted")
	}
	if _, err := newSession(nil, map[string]any{wizTokenField: 42}, "UA", time.Now()); err != ErrNoToken {
		t.Errorf("a non-string SNlM0e was accepted")
	}
}

// A logged-out visit to photos.google.com is served the public landing page rather than a
// redirect, so the landing URL looks signed in. The account cookie is the honest signal.
func TestNewSessionRequiresAnAccountCookie(t *testing.T) {
	loggedOut := []*network.Cookie{
		{Name: "NID", Value: "consent", Domain: ".google.com", Path: "/"},
		{Name: "SOCS", Value: "banner", Domain: ".google.com", Path: "/"},
	}

	_, err := newSession(loggedOut, map[string]any{wizTokenField: "token"}, "UA", time.Now())
	if !errors.Is(err, ErrNoAccountCookie) {
		t.Errorf("a logged-out cookie set gave err=%v, want ErrNoAccountCookie", err)
	}

	signedIn := append(loggedOut, &network.Cookie{Name: "SAPISID", Value: "real", Domain: ".google.com", Path: "/"})
	if _, err := newSession(signedIn, map[string]any{wizTokenField: "token"}, "UA", time.Now()); err != nil {
		t.Errorf("a signed-in cookie set was rejected: %v", err)
	}
}

// Jar scoping is what the Phase 0 spike could not express with a pasted Cookie: header.
// A ".google.com" cookie legitimately reaches every subdomain, exactly as in a browser;
// what the flat header could not reproduce is the host-scoped OSID that content hosts
// mint for themselves and that must not leak to the app host.
func TestJarPreservesHostScoping(t *testing.T) {
	const contentHost = "https://photos.fife.usercontent.google.com/x"

	session, err := newSession([]*network.Cookie{
		{Name: "SID", Value: "account-wide", Domain: ".google.com", Path: "/"},
		{Name: "OSID", Value: "content-host", Domain: "photos.fife.usercontent.google.com", Path: "/"},
	}, map[string]any{wizTokenField: "token"}, "UA", time.Now())
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}

	jar, err := session.Jar()
	if err != nil {
		t.Fatalf("Jar: %v", err)
	}

	appCookies := cookieNames(jar.Cookies(mustURL(t, "https://photos.google.com/")))
	if !appCookies["SID"] {
		t.Error("the account-wide cookie did not reach the app host")
	}
	if appCookies["OSID"] {
		t.Error("a host-scoped content cookie leaked to the app host")
	}

	contentCookies := cookieNames(jar.Cookies(mustURL(t, contentHost)))
	if !contentCookies["OSID"] || !contentCookies["SID"] {
		t.Errorf("the content host got %v, want both SID and OSID", contentCookies)
	}
}

func cookieNames(cookies []*http.Cookie) map[string]bool {
	present := make(map[string]bool, len(cookies))
	for _, cookie := range cookies {
		present[cookie.Name] = true
	}
	return present
}

func TestAcquireProfileIsExclusive(t *testing.T) {
	manager := NewManager(t.TempDir())

	if !manager.AcquireProfile() {
		t.Fatal("the first acquire failed")
	}
	if manager.AcquireProfile() {
		t.Fatal("the profile was acquired twice at once")
	}

	manager.ReleaseProfile()
	if !manager.AcquireProfile() {
		t.Fatal("the profile stayed locked after release")
	}
}

// A killed Chrome leaves a lock naming a host that will never come back, and Chrome will not
// open the profile again while it is there. Acquiring the profile has to clear it, or one
// killed browser ends every later one.
func TestAcquiringTheProfileClearsAKilledBrowsersLock(t *testing.T) {
	profileDir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(profileDir, "SingletonLock")
	if err := os.Symlink("a-container-that-is-gone-1234", abandoned); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(profileDir)
	if !manager.AcquireProfile() {
		t.Fatal("the profile could not be acquired")
	}
	defer manager.ReleaseProfile()

	if _, err := os.Lstat(abandoned); !os.IsNotExist(err) {
		t.Errorf("the abandoned lock survived: %v", err)
	}
}

// The web handler that closes the login browser redirects the moment this returns, so the
// in-flight flag has to be set by then — not merely soon after, on the warmup's goroutine.
func TestWarmupInBackgroundFlagsItselfBeforeReturning(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.AcquireProfile()

	release := make(chan struct{})
	finished := make(chan error, 1)
	manager.WarmupInBackground(t.Context(), func(_ *Session, err error) {
		<-release
		finished <- err
	})

	if !manager.Status().Warming {
		t.Fatal("the warmup was not flagged in flight by the time the call returned")
	}

	close(release)
	if err := <-finished; !errors.Is(err, ErrProfileBusy) {
		t.Fatalf("the warmup returned %v, want ErrProfileBusy", err)
	}
	if !eventually(func() bool { return !manager.Status().Warming }) {
		t.Fatal("the warmup stayed flagged in flight after it finished")
	}
}

func eventually(condition func() bool) bool {
	for range 200 {
		if condition() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// Shutdown cancels whatever warmup is in flight. That says nothing about the Google session,
// so it must not overwrite a healthy verdict with a failure and page the user on the way out.
func TestACancelledWarmupLeavesTheVerdictAlone(t *testing.T) {
	manager := NewManager(t.TempDir())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := manager.Warmup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled warmup returned %v, want context.Canceled", err)
	}

	status := manager.Status()
	if status.State != StateUnknown {
		t.Errorf("the session state became %q after a cancellation", status.State)
	}
	if !status.LastWarmup.IsZero() {
		t.Error("a cancelled warmup was recorded as a real check")
	}
	if status.UsingNoSandbox {
		t.Error("a cancellation was mistaken for a sandbox failure")
	}
}

// A check that could not be made is not evidence about the session. Recorded as auth_required it
// asks the user for the one remedy that cannot help, and buries the day Google really does sign
// them out under a fortnight of network blips saying the same thing.
func TestAWarmupThatCouldNotBeMadeIsNotASignedOutSession(t *testing.T) {
	manager := NewManager(t.TempDir())
	live := &Session{token: "token", userAgent: "Chrome"}
	manager.record(live, nil)

	manager.record(nil, errors.New("exec: \"google-chrome\": executable file not found in $PATH"))

	status := manager.Status()
	if status.State != StateWarmupFailed {
		t.Errorf("a browser that would not start left the session %q, want %q",
			status.State, StateWarmupFailed)
	}
	if _, held := manager.Session(); !held {
		t.Error("a failed check threw away a session it never used")
	}
	if status.LastSuccess.IsZero() {
		t.Error("the last success was forgotten by a check that never reached Google")
	}
}

// Google saying no is the one thing that does mean sign in again, and it must still say so.
func TestASignInPageLandingIsRecordedAsAuthRequired(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.record(&Session{token: "token"}, nil)

	manager.record(nil, fmt.Errorf("%w: landed on accounts.google.com", ErrAuthRequired))

	if state := manager.Status().State; state != StateAuthRequired {
		t.Errorf("a bounce to the sign-in page left the session %q, want %q", state, StateAuthRequired)
	}
	if _, held := manager.Session(); held {
		t.Error("a session Google refused was kept")
	}
}

func TestStatusStartsUnknown(t *testing.T) {
	status := NewManager(t.TempDir()).Status()
	if status.State != StateUnknown {
		t.Errorf("a fresh manager reports %q, want %q", status.State, StateUnknown)
	}
	if !status.LastWarmup.IsZero() || status.CookieCount != 0 {
		t.Errorf("a fresh manager already has history: %+v", status)
	}
}

func TestAvailableNamesEveryMissingBinary(t *testing.T) {
	t.Setenv("GPB_CHROME_PATH", filepath.Join(t.TempDir(), "no-such-chromium"))

	err := NewReauthStack(NewManager(t.TempDir()), t.TempDir()).Available()
	if !errors.Is(err, ErrReauthUnavailable) {
		t.Fatalf("Available() = %v, want ErrReauthUnavailable", err)
	}
	if !strings.Contains(err.Error(), "no-such-chromium") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// A refused start must leave nothing behind — the profile lock especially, since a leaked
// one would block every later keepalive warmup.
func TestStartRefusesCleanlyWithoutItsDependencies(t *testing.T) {
	t.Setenv("GPB_CHROME_PATH", filepath.Join(t.TempDir(), "no-such-chromium"))

	manager := NewManager(t.TempDir())
	stack := NewReauthStack(manager, t.TempDir())

	if err := stack.Start("session"); !errors.Is(err, ErrReauthUnavailable) {
		t.Fatalf("Start() = %v, want ErrReauthUnavailable", err)
	}
	if stack.Running() {
		t.Error("a refused start left the stack marked running")
	}
	if !manager.AcquireProfile() {
		t.Error("a refused start leaked the profile lock")
	}
}

// Owner is what tells a login browser somebody is using from one whose session has gone, so a
// stack with nothing running must name nobody — otherwise a stale owner keeps the next session
// locked out of a browser that no longer exists.
func TestAStackWithNothingRunningNamesNoOwner(t *testing.T) {
	stack := NewReauthStack(NewManager(t.TempDir()), t.TempDir())
	if owner := stack.Owner(); owner != "" {
		t.Errorf("a stack that never started names %q as its owner", owner)
	}

	stack.owner = "a session that has since gone"
	if owner := stack.Owner(); owner != "" {
		t.Errorf("a stack with no processes still names %q", owner)
	}
}

// A machine with its own screen skips the VNC chain entirely — on macOS it must, since
// Chrome there cannot draw into an X display at any price.
func TestHostDisplayNeedsOnlyABrowser(t *testing.T) {
	stack := NewReauthStack(NewManager(t.TempDir()), t.TempDir())
	stack.display = hostDisplay

	if got := stack.requiredBinaries(); len(got) != 1 {
		t.Errorf("requiredBinaries() = %v, want just the browser", got)
	}
	if got := stack.stackCommands(); len(got) != 1 || got[0].name != "chromium" {
		t.Errorf("stackCommands() spawned %d processes, want only the browser", len(got))
	}
	if stack.RemoteCanvas() {
		t.Error("a host-display stack claims a remote canvas")
	}
}

func TestVirtualDisplayNeedsTheWholeChain(t *testing.T) {
	stack := NewReauthStack(NewManager(t.TempDir()), t.TempDir())
	stack.display = virtualDisplay

	if got := stack.requiredBinaries(); len(got) != 4 {
		t.Errorf("requiredBinaries() = %v, want the full chain", got)
	}
	if got := stack.stackCommands(); len(got) != 4 {
		t.Errorf("stackCommands() spawned %d processes, want 4", len(got))
	}
	if !stack.RemoteCanvas() {
		t.Error("a virtual-display stack denies having a remote canvas")
	}
}

// The headful browser and chromedp's headless warmup share one profile, so they must
// agree on how the cookie jar is encrypted. If they drift, the warmup cannot decrypt what
// the login wrote and throws the entire jar away — a completed sign-in silently evaporates.
func TestHeadfulBrowserMatchesChromedpCookieEncryption(t *testing.T) {
	stack := NewReauthStack(NewManager(t.TempDir()), t.TempDir())

	args := stack.chromeArgs()
	for _, required := range []string{cookieEncryptionFlag, "--password-store=basic"} {
		if !slices.Contains(args, required) {
			t.Errorf("the login browser omits %s, so its cookies are unreadable by the warmup", required)
		}
	}
}

func TestChromePathPrefersTheExplicitOverride(t *testing.T) {
	t.Setenv("GPB_CHROME_PATH", "/somewhere/else/chrome")
	if got := chromePath(); got != "/somewhere/else/chrome" {
		t.Errorf("chromePath() = %q, want the override", got)
	}
}

func TestPortOf(t *testing.T) {
	if got := portOf("127.0.0.1:5900"); got != "5900" {
		t.Errorf("portOf = %q, want 5900", got)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return parsed
}

// The clean shutdown matters most for the warmup that was cancelled: a daemon going down is the
// browser most likely to be killed still holding cookies Google rotated and Chrome has not
// written. Asked for on the browser's own context, the shutdown is refused before it is sent.
func TestTheBrowserIsStillAskedToCloseAfterACancelledWarmup(t *testing.T) {
	abandoned, cancel := context.WithCancel(context.Background())
	cancel()

	closing, done := closeContext(abandoned)
	defer done()

	if err := closing.Err(); err != nil {
		t.Errorf("the shutdown context is already %v, so the browser is killed rather than closed", err)
	}
	deadline, hasDeadline := closing.Deadline()
	if !hasDeadline || time.Until(deadline) > browserCloseTimeout {
		t.Error("the shutdown context has no deadline of its own, so a hung browser would hang the caller")
	}
}
