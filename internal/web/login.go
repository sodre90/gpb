package web

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"gpb/internal/config"
)

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && s.sessions.touch(cookie.Value) {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, http.StatusOK, "")
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if code, problem, ok := s.checkLoginPassword(r); !ok {
		s.renderLogin(w, r, code, problem)
		return
	}

	id, err := s.sessions.create()
	if err != nil {
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}

	s.setSessionCookie(w, id, s.cfg.Web.SessionIdleTTL.Duration)
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (s *Server) checkLoginPassword(r *http.Request) (code int, problem string, ok bool) {
	s.guard.attempting.Lock()
	defer s.guard.attempting.Unlock()

	if notice, locked := s.lockedOut(); locked {
		return http.StatusTooManyRequests, notice, false
	}

	hash := s.passwordHash()
	if hash == "" {
		return http.StatusServiceUnavailable, "No password is configured. Run `gpb passwd` on the server.", false
	}

	matched, err := config.VerifyPassword(hash, r.FormValue("password"))
	if err != nil {
		log.Printf("web: stored password hash is unusable: %v", err)
		return http.StatusInternalServerError, "The stored password hash is unreadable. Run `gpb passwd` again.", false
	}
	if !matched {
		s.failLogin(r)
		return http.StatusUnauthorized, "Wrong password.", false
	}

	s.guard.recordSuccess()
	return http.StatusOK, "", true
}

// lockedOut is one wall in front of every form that takes the password. The login form is the
// obvious one, but a page that guesses its way past any of the others has the same prize, so
// they share a counter rather than each having their own.
func (s *Server) lockedOut() (notice string, locked bool) {
	remaining, locked := s.guard.lockedFor()
	if !locked {
		return "", false
	}
	return fmt.Sprintf("Too many failed attempts. Try again in %s.", remaining.Round(time.Minute)), true
}

// failLogin makes a wrong password cost a second before it is answered, which is what keeps
// guessing slow. Charging it only to failures leaks nothing a guesser cannot already see — the
// two answers are a 303 and a 401 — and spares the one person who knows the password a second
// of every visit.
func (s *Server) failLogin(r *http.Request) {
	s.wait(loginFailureDelay)

	if s.guard.recordFailure() {
		log.Printf("web: login locked out after %d consecutive failures", loginFailureLimit)
		s.notify("web_lockout", fmt.Sprintf("%d failed web logins; login locked for %s", loginFailureLimit, loginLockoutPeriod))
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	id := sessionIDFrom(r)
	if s.reauth.OwnedBy(id) {
		s.reauth.Stop("the session that started it logged out")
	}

	s.sessions.destroy(id)
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, message string) {
	render(w, status, "login", pageData{
		Title: "Log in",
		Error: message,
		Data:  safeNext(r.FormValue("next")),
	})
}

// Secure only when the UI is actually served over https: set on a page answering over http, the
// browser drops the cookie on the floor and nobody can log in at all. It is the session id's
// half of what TLS is for — a bearer token that never leaves an encrypted connection.
func (s *Server) setSessionCookie(w http.ResponseWriter, id string, idleTTL time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.Web.TLS.Enabled(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(idleTTL.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.Web.TLS.Enabled(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// safeNext keeps post-login redirects on this origin: anything that is not a plain
// absolute path is discarded rather than sanitised.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	return next
}
