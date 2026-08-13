package web

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"gpb/internal/auth"
	"gpb/internal/config"
)

const upstreamDialTimeout = 5 * time.Second

type reauthView struct {
	Status       statusView
	Running      bool
	OwnedByMe    bool
	StartedAt    string
	Unavailable  string
	RemoteCanvas bool
	IdleTimeout  string
}

func (s *Server) handleReauthPage(w http.ResponseWriter, r *http.Request) {
	s.renderReauth(w, r, http.StatusOK, "")
}

func (s *Server) handleReauthStart(w http.ResponseWriter, r *http.Request) {
	if code, problem, ok := s.confirmPassword(r); !ok {
		s.renderReauth(w, r, code, problem)
		return
	}

	err := s.reauth.Start(sessionIDFrom(r))
	switch {
	case err == nil:
		s.notify("reauth_started", "an interactive Google login browser was started from the web UI")
		http.Redirect(w, r, "/reauth", http.StatusSeeOther)
	case errors.Is(err, auth.ErrReauthRunning):
		s.renderReauth(w, r, http.StatusConflict, "A browser login is already running.")
	case errors.Is(err, auth.ErrProfileBusy):
		s.renderReauth(w, r, http.StatusConflict, "The browser profile is busy; try again in a minute.")
	case errors.Is(err, auth.ErrReauthUnavailable):
		s.renderReauth(w, r, http.StatusNotImplemented, "Browser login needs the container image: "+err.Error())
	default:
		log.Printf("web: starting re-auth: %v", err)
		s.renderReauth(w, r, http.StatusInternalServerError, "Could not start the login browser: "+err.Error())
	}
}

func (s *Server) handleReauthStop(w http.ResponseWriter, r *http.Request) {
	if !s.mayReachTheLoginBrowser(r) {
		s.renderReauth(w, r, http.StatusForbidden, "Another session is using the login browser.")
		return
	}

	s.reauth.Stop("finished from the web UI")
	http.Redirect(w, r, "/reauth?notice=Login+browser+closed.+Verifying+the+session.", http.StatusSeeOther)
}

// handleReauthSocket bridges noVNC to the loopback-only websockify. websockify is never
// reachable from the LAN; this proxy is the single door, and it opens only for the session
// that started the flow.
func (s *Server) handleReauthSocket(w http.ResponseWriter, r *http.Request) {
	if !s.mayReachTheLoginBrowser(r) {
		http.Error(w, "no login browser for this session", http.StatusForbidden)
		return
	}
	if !s.reauth.RemoteCanvas() {
		http.Error(w, "this host shows the login browser on its own screen", http.StatusNotImplemented)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be upgraded", http.StatusInternalServerError)
		return
	}

	upstream, err := net.DialTimeout("tcp", s.reauth.WebsockifyAddr(), upstreamDialTimeout)
	if err != nil {
		http.Error(w, "vnc bridge unreachable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	client, buffered, err := hijacker.Hijack()
	if err != nil {
		log.Printf("web: hijacking vnc connection: %v", err)
		return
	}
	defer client.Close()

	if err := r.Write(upstream); err != nil {
		log.Printf("web: forwarding vnc upgrade: %v", err)
		return
	}

	finished := make(chan struct{}, 2)
	go copyUntilDone(upstream, &activityReader{source: buffered, touch: s.reauth.Touch}, finished)
	go copyUntilDone(client, upstream, finished)
	<-finished
}

func copyUntilDone(destination io.Writer, source io.Reader, finished chan<- struct{}) {
	io.Copy(destination, source)
	finished <- struct{}{}
}

// activityReader reports inbound VNC traffic — keystrokes and pointer events — as user
// activity. Only the client-to-server direction counts: server frames can keep flowing
// from an animation nobody is watching.
type activityReader struct {
	source io.Reader
	touch  func()
}

func (a *activityReader) Read(buffer []byte) (int, error) {
	read, err := a.source.Read(buffer)
	if read > 0 {
		a.touch()
	}
	return read, err
}

// confirmPassword re-checks the password of a session that already has one, under the same
// lockout, the same delay and the same one-at-a-time rule as the login form itself. What it
// guards is worth as much as the login: a browser signed into the Google account.
func (s *Server) confirmPassword(r *http.Request) (code int, problem string, ok bool) {
	s.guard.attempting.Lock()
	defer s.guard.attempting.Unlock()

	if notice, locked := s.lockedOut(); locked {
		return http.StatusTooManyRequests, notice, false
	}
	if !s.passwordMatches(r.FormValue("password")) {
		s.failLogin(r)
		return http.StatusUnauthorized, "Wrong password.", false
	}

	s.guard.recordSuccess()
	return http.StatusOK, "", true
}

func (s *Server) passwordMatches(password string) bool {
	hash := s.passwordHash()
	if hash == "" {
		return false
	}

	matched, err := config.VerifyPassword(hash, password)
	if err != nil {
		log.Printf("web: stored password hash is unusable: %v", err)
		return false
	}
	return matched
}

func (s *Server) renderReauth(w http.ResponseWriter, r *http.Request, code int, problem string) {
	data := s.page(r, "Google")
	if problem != "" {
		data.Error = problem
	}

	data.Data = s.reauthView(r)
	render(w, code, "reauth", data)
}

// mayReachTheLoginBrowser decides who can watch a running login browser and close it. It is
// the session that started it — but that session can log out or time out while the browser
// runs on, and a browser nobody can reach is a signed-in Google window left open forever. So
// once its starter is gone, the next session to ask adopts it.
func (s *Server) mayReachTheLoginBrowser(r *http.Request) bool {
	owner := s.reauth.Owner()
	if owner == "" {
		return false
	}
	return owner == sessionIDFrom(r) || !s.sessions.stillOpen(owner)
}

// reauthView is built the same way for the whole page and for the fragment the socket asks for,
// because whose login browser it is decides what either of them is allowed to show.
func (s *Server) reauthView(r *http.Request) reauthView {
	view := reauthView{
		Status:       s.statusView(),
		Running:      s.reauth.Running(),
		OwnedByMe:    s.mayReachTheLoginBrowser(r),
		StartedAt:    humanTime(s.reauth.StartedAt()),
		RemoteCanvas: s.reauth.RemoteCanvas(),
		IdleTimeout:  auth.ReauthIdleTimeout().String(),
	}
	if err := s.reauth.Available(); err != nil {
		view.Unavailable = err.Error()
	}
	return view
}
