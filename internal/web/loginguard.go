package web

import (
	"sync"
	"time"
)

const (
	loginFailureDelay  = time.Second
	loginFailureLimit  = 10
	loginLockoutPeriod = 15 * time.Minute
)

// loginGuard throttles password attempts globally rather than per source address: on a LAN
// the source address is trivially spoofable and there is exactly one legitimate user, so
// per-IP accounting would only add bypasses.
type loginGuard struct {
	// attempting is held for a whole attempt — reading the lockout, verifying the hash and
	// recording the outcome — so that no two run at once. Without it the limit bounded rounds of
	// guessing rather than guesses: a hundred posts fired together all read a clean counter and
	// all got a full attempt. It also caps what argon2 can be asked to allocate, since one
	// verification in flight is 64 MiB rather than 64 MiB per request in flight.
	attempting sync.Mutex

	mu          sync.Mutex
	failures    int
	lockedUntil time.Time
}

func (g *loginGuard) lockedFor() (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	remaining := time.Until(g.lockedUntil)
	return remaining, remaining > 0
}

// recordFailure reports whether this attempt was the one that tripped the lockout, so the
// caller can raise the alarm exactly once.
func (g *loginGuard) recordFailure() bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.failures++
	if g.failures < loginFailureLimit {
		return false
	}

	g.failures = 0
	g.lockedUntil = time.Now().Add(loginLockoutPeriod)
	return true
}

func (g *loginGuard) recordSuccess() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures = 0
}
