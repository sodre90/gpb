package daemon

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gpb/internal/config"
	"gpb/internal/syncer"
)

// The guard is the whole point of the runner: two passes sharing one pool would race on the
// same .part files and double the request rate against Google.
func TestRunnerAdmitsOneRunAtATime(t *testing.T) {
	runner := NewRunner(config.Defaults(), nil, nil)

	if _, err := runner.claim("sync"); err != nil {
		t.Fatalf("claiming an idle runner: %v", err)
	}
	if _, err := runner.claim("album refresh"); !errors.Is(err, syncer.ErrRunInProgress) {
		t.Fatalf("a second claim returned %v, want ErrRunInProgress", err)
	}

	runner.release()

	if _, err := runner.claim("sync"); err != nil {
		t.Fatalf("the slot stayed held after release: %v", err)
	}
	runner.release()
}

func TestRunnerReportsWhatIsRunning(t *testing.T) {
	runner := NewRunner(config.Defaults(), nil, nil)

	if activity := runner.Activity(); activity != "" {
		t.Fatalf("an idle runner reports %q, want nothing", activity)
	}

	runner.claim("album refresh")
	if activity := runner.Activity(); activity != "album refresh" {
		t.Fatalf("a busy runner reports %q", activity)
	}

	runner.release()
	if activity := runner.Activity(); activity != "" {
		t.Fatalf("a finished runner still reports %q", activity)
	}
}

// A full disk is the failure a user is least likely to meet on their own: the daemon keeps
// running, the UI keeps loading, and every run from then on stops having downloaded nothing.
func TestAFullDiskIsPushedToTheUserAndOtherFailuresAreNot(t *testing.T) {
	runner := NewRunner(config.Defaults(), nil, nil)

	var events []string
	runner.notify = func(event, message string) { events = append(events, event+": "+message) }

	runner.reportIfOutOfDisk(nil)
	runner.reportIfOutOfDisk(errors.New("unexpected status 429 from the media host"))
	if len(events) != 0 {
		t.Fatalf("a run that did not run out of disk notified %v", events)
	}

	runner.reportIfOutOfDisk(fmt.Errorf("%w: 3.1 GB free, keeping 8.0 GB in reserve", syncer.ErrDiskFull))
	if len(events) != 1 {
		t.Fatalf("a run stopped by a full disk notified %v, want one event", events)
	}
	if !strings.HasPrefix(events[0], "disk_full: ") || !strings.Contains(events[0], "3.1 GB free") {
		t.Errorf("the notification does not say what happened: %q", events[0])
	}
}

// Stop cancels the run's context rather than only marking the slot free, so shutdown does
// not close the store out from under a pass still writing to it.
func TestStopCancelsTheRunInFlight(t *testing.T) {
	runner := NewRunner(config.Defaults(), nil, nil)

	ctx, err := runner.claim("sync")
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	go runner.release()

	runner.Stop()
	if ctx.Err() == nil {
		t.Fatal("the run context survived Stop")
	}
}
