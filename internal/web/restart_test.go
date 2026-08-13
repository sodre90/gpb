package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"gpb/internal/syncer"
)

// The restart is the second half of a save, so the address it sends the reader to has to be the
// one just written rather than the one this process started on — which after turning encryption
// on is the wrong scheme, and the whole reason the button exists.
func TestRestartingStopsTheDaemonAndSaysWhereItWillAnswer(t *testing.T) {
	t.Setenv("container", "podman")
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	stopped := make(chan struct{})
	server.stop = func() error { close(stopped); return nil }

	form := validSettingsForm()
	form.Set("external_url", "http://192.168.1.10:8090")
	form.Set("tls_mode", "self-signed")
	if recorder := postForm(handler, "/settings", form, cookie); recorder.Code != http.StatusSeeOther {
		t.Fatalf("turning encryption on: %d", recorder.Code)
	}

	recorder := postForm(handler, "/settings/restart", url.Values{}, cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("asking for a restart was answered with %d, want 200", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "https://192.168.1.10:8090") {
		t.Errorf("the page does not say where gpb will answer; body was:\n%s", body)
	}

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the page said gpb was stopping and nothing stopped it")
	}
}

// A button that stops the UI for good, on a machine where nothing will start it again, is a trap
// rather than a convenience.
func TestARestartIsOfferedOnlyWhereOneWouldComeBack(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	stopped := false
	server.stop = func() error { stopped = true; return nil }

	t.Setenv("container", "")
	if body := get(handler, "/settings", cookie).Body.String(); strings.Contains(body, "/settings/restart") {
		t.Error("a restart is offered where nothing would start gpb again")
	}
	recorder := postForm(handler, "/settings/restart", url.Values{}, cookie)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("asking for a restart anyway was answered with %d, want 400", recorder.Code)
	}
	if stopped {
		t.Fatal("gpb stopped itself with nothing to start it again")
	}

	t.Setenv("container", "podman")
	if body := get(handler, "/settings", cookie).Body.String(); !strings.Contains(body, "/settings/restart") {
		t.Error("no restart is offered in a container, where one is exactly what is wanted")
	}
}

// Restarting is survivable mid-run — the run is recorded as interrupted and the next one carries
// on from it — but that is not something to discover afterwards.
func TestTheRestartSaysWhenABackupWouldBeInterrupted(t *testing.T) {
	t.Setenv("container", "podman")
	server, _ := testServer(t)
	handler := server.Handler()
	cookie := login(t, handler)

	if body := get(handler, "/settings", cookie).Body.String(); strings.Contains(body, "A backup is running") {
		t.Error("nothing is running and the page says a backup is")
	}

	runsOf(server).nowRunning("sync", syncer.Progress{})
	if body := get(handler, "/settings", cookie).Body.String(); !strings.Contains(body, "A backup is running") {
		t.Error("a backup is running and the restart button says nothing about it")
	}
}
