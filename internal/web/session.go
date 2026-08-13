package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log"
	"time"

	"gpb/internal/store"
)

const (
	sessionCookieName = "gpb_session"
	sessionIDBytes    = 32
	absoluteTTL       = 90 * 24 * time.Hour
)

// sessionStore keeps sessions in the database rather than in the process, so that a redeploy —
// which for an appliance is a routine Tuesday — does not sign the one user out in the middle of
// what they were doing. Only a hash of the cookie value is written: the row is what a stolen
// database would be worth, and a hash makes it worth nothing.
type sessionStore struct {
	db      *store.Store
	idleTTL time.Duration
}

func newSessionStore(db *store.Store, idleTTL time.Duration) *sessionStore {
	return &sessionStore{db: db, idleTTL: idleTTL}
}

func (s *sessionStore) create() (string, error) {
	id, err := randomID()
	if err != nil {
		return "", err
	}

	idleSince, startedSince := s.cutoffs(time.Now())
	if err := s.db.DropExpiredWebSessions(idleSince, startedSince); err != nil {
		log.Printf("web: sweeping expired sessions: %v", err)
	}
	if err := s.db.StartWebSession(fingerprint(id), time.Now()); err != nil {
		return "", err
	}
	return id, nil
}

// touch validates a session id and slides its idle window in one pass, so every
// authenticated request renews the session it just used.
func (s *sessionStore) touch(id string) bool {
	if id == "" {
		return false
	}

	now := time.Now()
	idleSince, startedSince := s.cutoffs(now)
	renewed, err := s.db.RenewWebSession(fingerprint(id), now, idleSince, startedSince)
	if err != nil {
		log.Printf("web: renewing a session: %v", err)
		return false
	}
	return renewed
}

// stillOpen answers whether a session is one somebody could still be using, without renewing it.
// It is asked about a session other than the caller's own — whether whoever started the login
// browser is still around — so sliding its window here would keep a dead session alive.
func (s *sessionStore) stillOpen(id string) bool {
	if id == "" {
		return false
	}

	idleSince, startedSince := s.cutoffs(time.Now())
	live, err := s.db.WebSessionIsLive(fingerprint(id), idleSince, startedSince)
	if err != nil {
		log.Printf("web: reading a session: %v", err)
		return false
	}
	return live
}

func (s *sessionStore) destroy(id string) {
	if id == "" {
		return
	}
	if err := s.db.EndWebSession(fingerprint(id)); err != nil {
		log.Printf("web: ending a session: %v", err)
	}
}

// cutoffs are the two moments a session must be younger than: last used within the idle window,
// and started within the absolute one. The absolute limit is what stops a cookie somebody keeps
// warm from lasting for ever.
func (s *sessionStore) cutoffs(now time.Time) (idleSince, startedSince time.Time) {
	return now.Add(-s.idleTTL), now.Add(-absoluteTTL)
}

// fingerprint is what goes in the database in place of the cookie. A session id is a bearer
// token: anyone holding one is logged in, so the stored form has to be one that cannot be
// presented. It is not salted or stretched — the id is 32 random bytes, which is not guessable.
func fingerprint(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func randomID() (string, error) {
	raw := make([]byte, sessionIDBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
