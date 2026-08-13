package web

import (
	"log"
	"net/http"
	"os"
	"syscall"
	"time"

	"gpb/internal/config"
)

// handleRestart stops the daemon and leaves starting it again to whatever is running it. That is
// the whole mechanism: the settings this page writes are read once, at startup, and the alternative
// — rebuilding the scheduler, the limits and a listening socket in place — is a great deal of
// machinery for something a supervisor already does correctly.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if !supervised() {
		s.renderSettings(w, r, http.StatusBadRequest, "Nothing here would start gpb again, so it "+
			"has to be stopped and started the way it was started.")
		return
	}

	data := s.page(r, "Restarting")
	data.Data = s.addressToComeBackTo()
	render(w, http.StatusOK, "restarting", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	go s.stopForRestart()
}

// addressToComeBackTo is read from the file rather than from the config this process started with:
// the restart is usually the second half of a save, and after a change of encryption the address
// on disk is the one that will answer — which is not the one this page was fetched over.
func (s *Server) addressToComeBackTo() string {
	cfg, err := config.Load(s.cfg.DataDir())
	if err != nil {
		log.Printf("web: reading the config for the address to come back to: %v", err)
		return s.cfg.Web.ExternalURL
	}
	return cfg.Web.ExternalURL
}

func (s *Server) stopForRestart() {
	s.wait(restartDelay)
	log.Print("daemon: stopping, having been asked to restart from the settings page")
	if err := s.stop(); err != nil {
		log.Printf("web: asking for a restart: %v", err)
	}
}

// restartDelay is the page's head start. Shutdown lets an answer in flight finish, but the
// browser is being told where to come back to, and losing that message to a race would leave the
// reader on a dead connection with no address.
const restartDelay = 500 * time.Millisecond

// stopTheProcess is the SIGTERM `systemctl restart` would send, so a restart asked for here
// unwinds through the one shutdown path the daemon already has rather than a second one written
// for the occasion.
func stopTheProcess() error {
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

// supervised is gpb asking whether its own exit is somebody else's problem. Podman and Docker
// both set this; a daemon started by hand does not have it, and there a Restart button is a way
// to stop the UI with nothing left to start it again — so the page does not offer one.
func supervised() bool {
	return os.Getenv("container") != ""
}
