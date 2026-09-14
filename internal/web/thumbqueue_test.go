package web

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A jump along the timeline's rail lets go of a screen of thumbnails at once. The pictures
// asked for next must not wait behind them: a cancelled request has to give its turn back.
func TestCancelledThumbnailRequestsGiveTheirTurnBack(t *testing.T) {
	queue := newThumbQueue(8)

	abandoned, letGo := context.WithCancel(t.Context())
	var waiting sync.WaitGroup
	for range 80 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			queue.Wait(abandoned)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	letGo()
	waiting.Wait()

	start := time.Now()
	if err := queue.Wait(t.Context()); err != nil {
		t.Fatalf("a fresh request failed: %v", err)
	}
	// Eighty abandoned waits on a bare rate.Limiter left the next request 7.7 s of ghosts to
	// wait behind; with turns held only by the few at the bucket, it is under a second.
	if took := time.Since(start); took > time.Second {
		t.Errorf("a fresh request waited %v behind eighty cancelled ones", took.Round(time.Millisecond))
	}
}

// The bucket's burst still lets a fresh page fill at once: sixteen requests at a time pass
// without pacing, in whatever order the few turns admit them.
func TestAFreshPageStillGetsItsBurst(t *testing.T) {
	queue := newThumbQueue(8)

	start := time.Now()
	var waiting sync.WaitGroup
	for range thumbBurst {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			queue.Wait(t.Context())
		}()
	}
	waiting.Wait()
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Errorf("a burst of %d took %v, want it at once", thumbBurst, took.Round(time.Millisecond))
	}
}
