package store

import (
	"fmt"
	"strings"
)

// ReviewGroup is one album's worth of items waiting for a decision. The queue is grouped by
// album because that is how the decision is made: "did I want the new photos in this album?"
// is answerable, and "did I want these 300 photos from 40 albums?" is not.
type ReviewGroup struct {
	AlbumID   string
	Title     string
	Kind      AlbumKind
	Owner     string
	OwnedByMe bool
	Items     []MediaItem
}

// waitingForReview is what the queue shows, written once because the pill counts the same set.
// Flagged is not enough on its own: an item has to be in one of the two states the queue has
// verbs for and held by an album somebody follows, or the page will not list it. A pill counting
// a wider set is a badge saying "you have work waiting" that nothing the page offers can clear.
const waitingForReview = `
	mi.needs_review = 1
	AND mi.state = ?
	AND EXISTS (
		SELECT 1 FROM album_items ai
		JOIN albums a ON a.id = ai.album_id
		WHERE ai.media_key = mi.media_key AND a.sync_mode != 'none')`

// reviewStates are the two the queue has verbs for: a new item is approved or dismissed, and one
// Google has lost can only be acknowledged. An item flagged in any other state — a new one that
// has since been downloaded — has no question left to answer.
var reviewStates = []State{StateDiscovered, StateMissingUpstream}

// CountNeedingReview is the nav's pill. It counts items rather than groups: the pill answers
// "how much is waiting", and one album holding forty new photos is forty things to look at.
func (s *Store) CountNeedingReview() (int, error) {
	var waiting int
	for _, state := range reviewStates {
		var counted int
		query := `SELECT COUNT(*) FROM media_items mi WHERE ` + waitingForReview
		if err := s.db.QueryRow(query, string(state)).Scan(&counted); err != nil {
			return 0, fmt.Errorf("counting items needing review: %w", err)
		}
		waiting += counted
	}
	return waiting, nil
}

// KeysWaitingForReview narrows a set of media keys to those the queue is actually asking about.
// A decision is a decision about something the page offered: a request naming anything else is
// settling an item nobody was asked about, and for 'approve' that means selecting a photo for
// download that the user never saw.
func (s *Store) KeysWaitingForReview(mediaKeys []string) ([]string, error) {
	if len(mediaKeys) == 0 {
		return nil, nil
	}

	var waiting []string
	for _, state := range reviewStates {
		found, err := s.keysWaitingInState(state, mediaKeys)
		if err != nil {
			return nil, err
		}
		waiting = append(waiting, found...)
	}
	return waiting, nil
}

func (s *Store) keysWaitingInState(state State, mediaKeys []string) ([]string, error) {
	arguments := make([]any, 0, len(mediaKeys)+1)
	arguments = append(arguments, string(state))
	for _, key := range mediaKeys {
		arguments = append(arguments, key)
	}

	rows, err := s.db.Query(`
		SELECT mi.media_key FROM media_items mi
		WHERE `+waitingForReview+`
		  AND mi.media_key IN (?`+strings.Repeat(", ?", len(mediaKeys)-1)+`)`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("checking what is waiting for review: %w", err)
	}
	defer rows.Close()

	var waiting []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		waiting = append(waiting, key)
	}
	return waiting, rows.Err()
}

// ItemsNeedingReview returns the flagged items in one state, grouped by the album they were
// found in. State separates the queue's two halves, which share the flag but need opposite
// verbs: a new item in a picked album is approved or dismissed, while one Google has lost can
// only be acknowledged.
//
// Albums nobody follows are left out. Their items are flagged all the same, and asking about
// photos in an album the user has already declined would be a queue that never empties.
func (s *Store) ItemsNeedingReview(state State) ([]ReviewGroup, error) {
	items, err := s.queryItems(`
		SELECT `+itemColumns+` FROM media_items mi
		WHERE `+waitingForReview, string(state))
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	byKey := make(map[string]MediaItem, len(items))
	for _, item := range items {
		byKey[item.MediaKey] = item
	}
	return s.groupForReview(state, byKey)
}

func (s *Store) groupForReview(state State, byKey map[string]MediaItem) ([]ReviewGroup, error) {
	rows, err := s.db.Query(`
		SELECT ai.media_key, a.id, a.title, a.kind, a.owner_name, a.owner_is_account
		FROM album_items ai
		JOIN albums a ON a.id = ai.album_id
		JOIN media_items mi ON mi.media_key = ai.media_key
		WHERE mi.needs_review = 1 AND mi.state = ? AND a.sync_mode != 'none'
		ORDER BY a.title, a.id, mi.captured_at DESC, ai.media_key`, string(state))
	if err != nil {
		return nil, fmt.Errorf("grouping the review queue: %w", err)
	}
	defer rows.Close()

	var groups []ReviewGroup
	for rows.Next() {
		var mediaKey string
		var group ReviewGroup
		if err := rows.Scan(&mediaKey, &group.AlbumID, &group.Title, &group.Kind,
			&group.Owner, &group.OwnedByMe); err != nil {
			return nil, fmt.Errorf("scanning a review group: %w", err)
		}

		item, known := byKey[mediaKey]
		if !known {
			continue
		}
		if len(groups) == 0 || groups[len(groups)-1].AlbumID != group.AlbumID {
			groups = append(groups, group)
		}
		last := &groups[len(groups)-1]
		last.Items = append(last.Items, item)
	}
	return groups, rows.Err()
}

// FlagForReview marks items as wanting a decision. It leaves selection alone: an item in a
// 'picked' album is unselected until the user says otherwise, and that is exactly what makes
// this queue necessary rather than merely informative.
func (s *Store) FlagForReview(mediaKeys []string) error {
	if len(mediaKeys) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("flagging items for review: %w", err)
	}
	defer tx.Rollback()

	statement, err := tx.Prepare(`UPDATE media_items SET needs_review = 1 WHERE media_key = ?`)
	if err != nil {
		return fmt.Errorf("flagging items for review: %w", err)
	}
	defer statement.Close()

	for _, mediaKey := range mediaKeys {
		if _, err := statement.Exec(mediaKey); err != nil {
			return fmt.Errorf("flagging an item for review: %w", err)
		}
	}
	return tx.Commit()
}

// ClearNeedsReview settles items without touching anything else about them. Acknowledging a
// photo Google has lost must not delete the copy on disk, and dismissing a new one must not
// mark it downloaded — the flag is the only thing this queue owns.
func (s *Store) ClearNeedsReview(mediaKeys []string) error {
	if len(mediaKeys) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("clearing review flags: %w", err)
	}
	defer tx.Rollback()

	statement, err := tx.Prepare(`UPDATE media_items SET needs_review = 0 WHERE media_key = ?`)
	if err != nil {
		return fmt.Errorf("clearing review flags: %w", err)
	}
	defer statement.Close()

	for _, mediaKey := range mediaKeys {
		if _, err := statement.Exec(mediaKey); err != nil {
			return fmt.Errorf("clearing the review flag on an item: %w", err)
		}
	}
	return tx.Commit()
}
