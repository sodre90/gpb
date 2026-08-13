package syncer

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"gpb/internal/gphotos"
)

// Backoff spaces out retries. The defaults are deliberately timid: a personal backup gains
// nothing from hammering a server that is already unhappy.
type Backoff struct {
	Base   time.Duration
	Max    time.Duration
	Jitter float64
}

func DefaultBackoff() Backoff {
	return Backoff{Base: time.Second, Max: 2 * time.Minute, Jitter: 0.3}
}

// Delay grows exponentially with jitter, except when the server named its own delay, which
// is obeyed as given. Capping Retry-After would mean retrying sooner than we were asked —
// exactly the behaviour that earns a block. The run's context bounds the total wait instead.
func (b Backoff) Delay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}

	delay := b.Base << min(attempt, 20)
	if delay > b.Max || delay <= 0 {
		delay = b.Max
	}
	return delay + time.Duration(rand.Float64()*b.Jitter*float64(delay))
}

// Retryable says whether another attempt could plausibly succeed. A cancelled context never
// is: the run is over, and retrying would only delay the shutdown.
func Retryable(err error) bool {
	switch {
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, gphotos.ErrSessionRejected), errors.Is(err, gphotos.ErrProtocolDrift):
		return false
	case errors.Is(err, gphotos.ErrUnknownContentHost):
		return false
	case errors.Is(err, gphotos.ErrTruncated), errors.Is(err, gphotos.ErrRangeIgnored):
		return true
	case errors.Is(err, gphotos.ErrSignedURLRejected):
		return true
	}

	var httpErr *gphotos.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Retryable()
	}
	return true
}

func retryAfterOf(err error) time.Duration {
	var httpErr *gphotos.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.RetryAfter
	}
	return 0
}

// Breaker aborts a run after enough consecutive failures. Its job is to distinguish "this
// item is bad" from "the far end is broken": the former is one failure among many successes,
// the latter is every attempt failing in a row, and grinding through a whole library in that
// state is how an account attracts attention.
type Breaker struct {
	threshold int

	mu           sync.Mutex
	consecutive  int
	trippedCause error
}

func NewBreaker(threshold int) *Breaker {
	return &Breaker{threshold: threshold}
}

func (b *Breaker) Succeed() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
}

// Fail records a failure and reports whether the breaker has just tripped.
func (b *Breaker) Fail(cause error) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.consecutive++
	if b.consecutive < b.threshold || b.trippedCause != nil {
		return false
	}
	b.trippedCause = cause
	return true
}

func (b *Breaker) Tripped() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.trippedCause
}
