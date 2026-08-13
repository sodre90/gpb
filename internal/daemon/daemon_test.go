package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpb/internal/config"
)

// A notification that names a URL nobody can reach is worse than one that does not: the user
// follows it, gets nothing, and learns to distrust the next one.
func TestTheReauthNotificationOmitsAnAddressNobodyConfigured(t *testing.T) {
	daemon := &Daemon{cfg: config.Defaults()}

	if link := daemon.reauthLink(); strings.Contains(link, "http") {
		t.Errorf("an unconfigured external_url produced the link %q", link)
	}

	daemon.cfg.Web.ExternalURL = "http://photos.example/"
	if link := daemon.reauthLink(); link != "http://photos.example/reauth" {
		t.Errorf("the link was %q, want the trailing slash collapsed", link)
	}
}

// Notification is never allowed to hold up the daemon, and the shape that broke that promise is
// the most ordinary thing a hook script does: back something off into the background. The child
// inherits stdout, so the wait outlives the hook itself — on the goroutine running the backup.
func TestANotifyHookThatLeavesAChildHoldingStdoutIsAbandoned(t *testing.T) {
	hook := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 60 &\necho notified\n"), 0o755); err != nil {
		t.Fatalf("writing the hook: %v", err)
	}

	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("loading a config: %v", err)
	}
	cfg.Notify.Command = hook
	daemon := &Daemon{cfg: cfg}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		daemon.Notify("test", "the hook has already exited")
	}()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the notify hook held its caller after the command itself had exited")
	}
}
