package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	reauthDisplay     = ":99"
	reauthScreen      = "1280x900x24"
	vncAddr           = "127.0.0.1:5900"
	websockifyAddr    = "127.0.0.1:6080"
	reauthIdleTimeout = 15 * time.Minute
	settleDelay       = 700 * time.Millisecond
	livenessGrace     = 2 * time.Second
	terminateGrace    = 5 * time.Second
	loginStartURL     = "https://accounts.google.com/ServiceLogin?continue=https%3A%2F%2Fphotos.google.com%2F"
)

// cookieEncryptionFlag must match what browserOptions passes to the warmup browser. On
// macOS the cookie jar is encrypted with a key from the Keychain
// unless this flag substitutes a fixed mock key; if the two browsers disagree, the one
// that cannot decrypt the jar silently discards every cookie in it — so a completed login
// vanishes the moment the next warmup runs. It is a no-op off macOS, and is deliberately
// unconditional so the two processes can never drift apart.
const cookieEncryptionFlag = "--use-mock-keychain"

var (
	ErrProfileBusy       = errors.New("the browser profile is in use by another operation")
	ErrReauthRunning     = errors.New("a re-auth flow is already running")
	ErrReauthNotOwner    = errors.New("this session did not start the re-auth flow")
	ErrReauthUnavailable = errors.New("the browser login stack is not installed")
)

// displayMode decides how the login browser reaches the user's eyes. A headless server
// needs the virtual-display-and-VNC chain; a workstation already has a screen, and on
// macOS it has no choice — Chrome there is a Cocoa app that cannot draw into an X display,
// so the VNC chain is not merely redundant but impossible.
type displayMode int

const (
	virtualDisplay displayMode = iota
	hostDisplay
)

func ReauthIdleTimeout() time.Duration { return reauthIdleTimeout }

func detectDisplayMode() displayMode {
	if runtime.GOOS == "darwin" {
		return hostDisplay
	}
	return virtualDisplay
}

// managedProcess pairs a child with a channel closed when it exits, so both the liveness
// check and the teardown can wait on it without racing over a second cmd.Wait.
type managedProcess struct {
	name   string
	cmd    *exec.Cmd
	exited chan struct{}
}

// ReauthStack runs a headful browser on the user's behalf and, where the machine has no
// screen of its own, exposes it over VNC on loopback only. It holds a fully authenticated
// Google browser while running, so it stays off until asked and is torn down eagerly.
type ReauthStack struct {
	manager    *Manager
	profileDir string
	display    displayMode

	mu           sync.Mutex
	processes    []*managedProcess
	owner        string
	startedAt    time.Time
	lastActivity time.Time
	idleCancel   context.CancelFunc
	onClosed     func(reason string)
}

func NewReauthStack(manager *Manager, profileDir string) *ReauthStack {
	return &ReauthStack{manager: manager, profileDir: profileDir, display: detectDisplayMode()}
}

func (r *ReauthStack) OnClosed(handler func(reason string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onClosed = handler
}

func (r *ReauthStack) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.processes) > 0
}

func (r *ReauthStack) OwnedBy(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.processes) > 0 && r.owner == sessionID
}

// Owner names the web session that started the running browser, or nothing when none is
// running. It lets the caller ask whether that session still exists, which is what tells an
// abandoned browser from one somebody is still using.
func (r *ReauthStack) Owner() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.processes) == 0 {
		return ""
	}
	return r.owner
}

func (r *ReauthStack) StartedAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startedAt
}

func (r *ReauthStack) WebsockifyAddr() string { return websockifyAddr }

// RemoteCanvas reports whether the login browser can be viewed from another device. When
// false the browser opens on this machine's own screen and the user must be sitting at it.
func (r *ReauthStack) RemoteCanvas() bool { return r.display == virtualDisplay }

// Available reports whether every process in the stack is installed, so a missing
// dependency is one clear message instead of a half-spawned stack and a raw exec error.
func (r *ReauthStack) Available() error {
	var missing []string
	for _, binary := range r.requiredBinaries() {
		if _, err := exec.LookPath(binary); err != nil {
			missing = append(missing, binary)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing %s", ErrReauthUnavailable, strings.Join(missing, ", "))
	}
	return nil
}

func (r *ReauthStack) requiredBinaries() []string {
	if r.display == hostDisplay {
		return []string{chromePath()}
	}
	return []string{"Xvfb", chromePath(), "x11vnc", "websockify"}
}

func (r *ReauthStack) Start(sessionID string) error {
	if err := r.Available(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.processes) > 0 {
		return ErrReauthRunning
	}
	if !r.manager.AcquireProfile() {
		return ErrProfileBusy
	}

	if err := r.spawnAll(); err != nil {
		r.teardownLocked()
		r.manager.ReleaseProfile()
		return err
	}

	r.owner = sessionID
	r.startedAt = time.Now()
	r.lastActivity = r.startedAt

	idleCtx, cancel := context.WithCancel(context.Background())
	r.idleCancel = cancel
	go r.watchIdle(idleCtx)

	log.Printf("auth: re-auth browser started by web session %s", shortID(sessionID))
	return nil
}

func (r *ReauthStack) spawnAll() error {
	for _, step := range r.stackCommands() {
		step.cmd.Env = r.childEnv()
		step.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		step.cmd.Stdout, step.cmd.Stderr = os.Stdout, os.Stderr

		if err := step.cmd.Start(); err != nil {
			return fmt.Errorf("starting %s: %w", step.name, err)
		}
		r.processes = append(r.processes, step)
		go func(process *managedProcess) {
			process.cmd.Wait()
			close(process.exited)
		}(step)

		time.Sleep(settleDelay)
	}

	return r.assertAllAlive()
}

func (r *ReauthStack) childEnv() []string {
	if r.display == hostDisplay {
		return os.Environ()
	}
	return append(os.Environ(), "DISPLAY="+reauthDisplay)
}

// assertAllAlive turns the common silent failure — a child that starts and dies a moment
// later — into an error at the point the user asked for the flow, instead of a blank VNC
// canvas with nothing to explain it.
func (r *ReauthStack) assertAllAlive() error {
	time.Sleep(livenessGrace)

	for _, process := range r.processes {
		select {
		case <-process.exited:
			return fmt.Errorf("%s exited immediately (%v)", process.name, process.cmd.ProcessState)
		default:
		}
	}
	return nil
}

func (r *ReauthStack) stackCommands() []*managedProcess {
	browser := newManagedProcess("chromium", exec.Command(chromePath(), r.chromeArgs()...))
	if r.display == hostDisplay {
		return []*managedProcess{browser}
	}

	return []*managedProcess{
		newManagedProcess("Xvfb", exec.Command("Xvfb", reauthDisplay, "-screen", "0", reauthScreen, "-nolisten", "tcp")),
		browser,
		newManagedProcess("x11vnc", exec.Command("x11vnc",
			"-display", reauthDisplay,
			"-rfbport", portOf(vncAddr),
			"-localhost", "-forever", "-shared", "-nopw", "-quiet",
		)),
		newManagedProcess("websockify", exec.Command("websockify", websockifyAddr, vncAddr)),
	}
}

// chromeArgs mirrors the headless warmup's sandbox decision: where the container cannot
// give Chromium a usable sandbox, the headful browser cannot start either.
func (r *ReauthStack) chromeArgs() []string {
	args := []string{
		"--user-data-dir=" + r.profileDir,
		"--password-store=basic",
		cookieEncryptionFlag,
		"--disable-blink-features=AutomationControlled",
		"--no-first-run",
		"--no-default-browser-check",
		"--window-position=0,0",
		"--window-size=1280,900",
	}
	if r.display == virtualDisplay {
		args = append(args, "--start-maximized")
	}
	if r.manager.Status().UsingNoSandbox {
		args = append(args, "--no-sandbox")
	}
	return append(args, loginStartURL)
}

func newManagedProcess(name string, cmd *exec.Cmd) *managedProcess {
	return &managedProcess{name: name, cmd: cmd, exited: make(chan struct{})}
}

// startVirtualDisplay opens a screen for a headful browser on a machine that has none. The
// caller stops it when the browser is done with it; a display that outlived its browser would
// deny the next one its number.
func startVirtualDisplay(display string) (func(), error) {
	screen := newManagedProcess("Xvfb",
		exec.Command("Xvfb", display, "-screen", "0", reauthScreen, "-nolisten", "tcp"))
	screen.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	screen.cmd.Stdout, screen.cmd.Stderr = os.Stdout, os.Stderr

	if err := screen.cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting Xvfb on %s: %w", display, err)
	}
	go func() {
		screen.cmd.Wait()
		close(screen.exited)
	}()

	time.Sleep(settleDelay)
	return func() { terminate(screen) }, nil
}

// Touch records VNC traffic so an active login is not torn down under the user.
func (r *ReauthStack) Touch() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastActivity = time.Now()
}

func (r *ReauthStack) Stop(reason string) {
	r.mu.Lock()
	running := len(r.processes) > 0
	if running {
		r.teardownLocked()
	}
	handler := r.onClosed
	r.mu.Unlock()

	if !running {
		return
	}

	r.manager.ReleaseProfile()
	log.Printf("auth: re-auth browser stopped (%s)", reason)
	if handler != nil {
		handler(reason)
	}
}

func (r *ReauthStack) teardownLocked() {
	if r.idleCancel != nil {
		r.idleCancel()
		r.idleCancel = nil
	}

	for index := len(r.processes) - 1; index >= 0; index-- {
		terminate(r.processes[index])
	}
	r.processes = nil
	r.owner = ""
	r.startedAt = time.Time{}
}

func (r *ReauthStack) watchIdle(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			idle := time.Since(r.lastActivity)
			r.mu.Unlock()

			if idle >= reauthIdleTimeout {
				r.Stop("idle timeout")
				return
			}
		}
	}
}

func terminate(process *managedProcess) {
	if process.cmd.Process == nil {
		return
	}

	syscall.Kill(-process.cmd.Process.Pid, syscall.SIGTERM)

	select {
	case <-process.exited:
	case <-time.After(terminateGrace):
		syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL)
		<-process.exited
	}
}

// macChromeCandidates are where a desktop Chrome actually lives on macOS; the bare name
// "chromium" is never on a Mac's PATH.
var macChromeCandidates = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
	"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
}

func chromePath() string {
	if path := os.Getenv("GPB_CHROME_PATH"); path != "" {
		return path
	}
	if runtime.GOOS == "darwin" {
		for _, candidate := range macChromeCandidates {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return "chromium"
}

func portOf(addr string) string {
	for index := len(addr) - 1; index >= 0; index-- {
		if addr[index] == ':' {
			return addr[index+1:]
		}
	}
	return addr
}

func shortID(sessionID string) string {
	if len(sessionID) <= 8 {
		return sessionID
	}
	return sessionID[:8] + "…"
}
