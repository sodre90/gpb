package daemon

import (
	"errors"
	"fmt"
	"os"
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

// Linking needs no browser, which is what lets the Review page's button and the end of a backup
// both run it: a runner built with no Google session at all has to be able to finish the pass.
func TestLinkingMakesRepeatedCopiesOneFileAndCountsThemAgain(t *testing.T) {
	daemon := scheduledDaemon(t, "03:30")
	first := backUpFile(t, daemon.store, "album-key", "the same photo")
	second := backUpFile(t, daemon.store, "timeline-key", "the same photo")

	if _, counted := daemon.runner.SeparateCopies(); counted {
		t.Fatal("a runner that has never counted claims a count")
	}
	daemon.runner.CountCopies(t.Context())
	if copies, _ := daemon.runner.SeparateCopies(); copies.Separate != 1 {
		t.Fatalf("counted %+v before linking, want one extra file", copies)
	}

	if err := daemon.runner.StartLinking("test"); err != nil {
		t.Fatalf("starting the pass: %v", err)
	}
	daemon.runner.inFlight.Wait()

	firstInfo, _ := os.Stat(first)
	secondInfo, _ := os.Stat(second)
	if !os.SameFile(firstInfo, secondInfo) {
		t.Error("the two copies are still two files")
	}
	if copies, _ := daemon.runner.SeparateCopies(); copies.Separate != 0 {
		t.Errorf("counted %+v after linking, want nothing left", copies)
	}
	if activity := daemon.runner.Activity(); activity != "" {
		t.Errorf("the runner still reports %q after the pass", activity)
	}
}
