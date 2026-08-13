package store

import (
	"fmt"
	"strings"
)

// AlbumPage is one screen of an album, oldest capture first so paging through matches the
// order the photos were taken rather than the order Google happened to return them.
func (s *Store) AlbumPage(albumID string, offset, limit int) ([]MediaItem, error) {
	return s.queryItems(`
		SELECT `+itemColumns+` FROM media_items
		WHERE media_key IN (SELECT media_key FROM album_items WHERE album_id = ?)
		ORDER BY captured_at, media_key
		LIMIT ? OFFSET ?`, albumID, limit, offset)
}

// EveryItemPage is one screen of the whole account, newest capture first. It answers the
// question no album can: an album page shows what one album holds, and half this library is in
// no album at all. Newest first because someone browsing everything is looking for something
// recent far more often than for something from 2009.
func (s *Store) EveryItemPage(offset, limit int) ([]MediaItem, error) {
	return s.queryItems(`
		SELECT `+itemColumns+` FROM media_items
		ORDER BY captured_at DESC, media_key
		LIMIT ? OFFSET ?`, limit, offset)
}

func (s *Store) EveryItemCount() (int, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM media_items`).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting every item: %w", err)
	}
	return count, nil
}

func (s *Store) AlbumItemCount(albumID string) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM album_items WHERE album_id = ?`, albumID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting album items: %w", err)
	}
	return count, nil
}

// SelectionIn reports which of an album's items are picked. It returns a set rather than a
// per-item column so the grid can render a page without a join per row, and so the caller
// can answer "is this picked" for items the page does not show.
func (s *Store) SelectionIn(albumID string) (map[string]bool, error) {
	rows, err := s.db.Query(`
		SELECT ai.media_key FROM album_items ai
		JOIN media_selection ms ON ms.media_key = ai.media_key
		WHERE ai.album_id = ? AND ms.selected = 1`, albumID)
	if err != nil {
		return nil, fmt.Errorf("reading the album selection: %w", err)
	}
	defer rows.Close()

	selected := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		selected[key] = true
	}
	return selected, rows.Err()
}

// SetSelection marks a batch of items in one transaction, because the grid sends a
// shift-click range as a single request and half an applied range is a worse answer than
// none. Keys naming items this store has never seen are refused by the foreign key.
func (s *Store) SetSelection(mediaKeys []string, selected bool) error {
	if len(mediaKeys) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("starting the selection update: %w", err)
	}
	defer tx.Rollback()

	statement, err := tx.Prepare(`
		INSERT INTO media_selection (media_key, selected) VALUES (?, ?)
		ON CONFLICT(media_key) DO UPDATE SET selected = excluded.selected`)
	if err != nil {
		return fmt.Errorf("preparing the selection update: %w", err)
	}
	defer statement.Close()

	for _, key := range mediaKeys {
		if _, err := statement.Exec(key, selected); err != nil {
			return fmt.Errorf("selecting %s: %w", key, err)
		}
	}
	return tx.Commit()
}

// SelectWholeAlbum runs "select all" server-side. On a ten-thousand-item album the browser
// has loaded 200 keys and must never need the rest just to tick a box.
func (s *Store) SelectWholeAlbum(albumID string, selected bool) error {
	_, err := s.db.Exec(`
		INSERT INTO media_selection (media_key, selected)
		SELECT media_key, ? FROM album_items WHERE album_id = ?
		ON CONFLICT(media_key) DO UPDATE SET selected = excluded.selected`,
		selected, albumID)
	if err != nil {
		return fmt.Errorf("selecting the whole album: %w", err)
	}
	return nil
}

// MediaKeysIn filters a caller-supplied list down to the keys that really belong to the
// album. The grid posts keys from a page the browser rendered, and a request must not be
// able to mark items in an album it was not looking at.
func (s *Store) MediaKeysIn(albumID string, mediaKeys []string) ([]string, error) {
	if len(mediaKeys) == 0 {
		return nil, nil
	}

	arguments := make([]any, 0, len(mediaKeys)+1)
	arguments = append(arguments, albumID)
	for _, key := range mediaKeys {
		arguments = append(arguments, key)
	}

	query := `SELECT media_key FROM album_items WHERE album_id = ? AND media_key IN (?` +
		strings.Repeat(", ?", len(mediaKeys)-1) + `)`

	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("checking album membership: %w", err)
	}
	defer rows.Close()

	var belonging []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		belonging = append(belonging, key)
	}
	return belonging, rows.Err()
}
