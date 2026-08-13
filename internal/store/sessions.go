package store

import (
	"fmt"
	"time"
)

// The web layer decides what a session's lifetime is and hands the cutoffs down; this file only
// keeps them. Rows past their cutoff are not deleted on sight — a query that has to ignore them
// anyway costs nothing extra, and sweeping is left to StartWebSession, which runs at a login.

func (s *Store) StartWebSession(idHash string, now time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO web_sessions (id_hash, created_at, last_seen) VALUES (?, ?, ?)`,
		idHash, formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("starting a web session: %w", err)
	}
	return nil
}

// RenewWebSession slides a live session's idle window and reports whether there was one to slide,
// in a single statement: asking first and writing after would let a session expire between the
// two, and the answer the caller acts on is the write's own.
func (s *Store) RenewWebSession(idHash string, now, idleSince, startedSince time.Time) (bool, error) {
	result, err := s.db.Exec(`
		UPDATE web_sessions SET last_seen = ?
		WHERE id_hash = ? AND last_seen >= ? AND created_at >= ?`,
		formatTime(now), idHash, formatTime(idleSince), formatTime(startedSince))
	if err != nil {
		return false, fmt.Errorf("renewing a web session: %w", err)
	}

	renewed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("renewing a web session: %w", err)
	}
	return renewed > 0, nil
}

// WebSessionIsLive answers for a session other than the caller's own — whether whoever started
// the login browser is still around — so unlike RenewWebSession it leaves the idle window alone.
func (s *Store) WebSessionIsLive(idHash string, idleSince, startedSince time.Time) (bool, error) {
	var live int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM web_sessions
		WHERE id_hash = ? AND last_seen >= ? AND created_at >= ?`,
		idHash, formatTime(idleSince), formatTime(startedSince)).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("reading a web session: %w", err)
	}
	return live > 0, nil
}

func (s *Store) EndWebSession(idHash string) error {
	if _, err := s.db.Exec(`DELETE FROM web_sessions WHERE id_hash = ?`, idHash); err != nil {
		return fmt.Errorf("ending a web session: %w", err)
	}
	return nil
}

func (s *Store) DropExpiredWebSessions(idleSince, startedSince time.Time) error {
	_, err := s.db.Exec(`
		DELETE FROM web_sessions WHERE last_seen < ? OR created_at < ?`,
		formatTime(idleSince), formatTime(startedSince))
	if err != nil {
		return fmt.Errorf("dropping expired web sessions: %w", err)
	}
	return nil
}
