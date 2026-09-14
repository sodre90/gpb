package store

import (
	"database/sql"
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

// MonthCount is how many of a grid's items were captured in one month, in the grid's own order.
// Month is "2006-01" in the server's local time — the same clock the captions read — or empty
// for items Google reported no capture date for.
type MonthCount struct {
	Month string
	Count int
}

// EveryItemMonths is the whole-library grid folded by month, newest first, which is what lets a
// page lay the whole library out before it has fetched any of it: the count of each month says
// how tall the month is, and the sum of the months above one says where its items start.
//
// The undated bucket lands where the item order puts undated items — last here, where the order
// is descending, and first in an album — so the two never disagree about where a cell is.
func (s *Store) EveryItemMonths() ([]MonthCount, error) {
	return s.queryMonths(`
		SELECT ` + captureMonth + ` AS month, COUNT(*) FROM media_items
		GROUP BY month ORDER BY month DESC`)
}

// AlbumMonths is one album folded the same way, oldest first, to match AlbumPage.
func (s *Store) AlbumMonths(albumID string) ([]MonthCount, error) {
	return s.queryMonths(`
		SELECT `+captureMonth+` AS month, COUNT(*) FROM media_items
		WHERE media_key IN (SELECT media_key FROM album_items WHERE album_id = ?)
		GROUP BY month ORDER BY month`, albumID)
}

// captureMonth folds a capture time to its local-time month. Local because the captions are
// local (humanDate), and a header saying August over a cell captioned 31 July would look like a
// bug. Local time is a monotonic view of the stored instants, so the months stay contiguous in
// the order the grids use.
const captureMonth = `strftime('%Y-%m', captured_at, 'localtime')`

func (s *Store) queryMonths(query string, arguments ...any) ([]MonthCount, error) {
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("counting items by month: %w", err)
	}
	defer rows.Close()

	var months []MonthCount
	for rows.Next() {
		var month sql.NullString
		var count int
		if err := rows.Scan(&month, &count); err != nil {
			return nil, fmt.Errorf("reading a month's count: %w", err)
		}
		months = append(months, MonthCount{Month: month.String, Count: count})
	}
	return months, rows.Err()
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

// SelectRange marks every item of an album between two of its cells, in the album's own order
// and including both ends. A shift-click range used to be resolved in the browser from the cells
// it had, which was every cell on the page; a grid that holds only the cells near the viewport
// cannot see the ones in between, so the store, which can, does the counting. The ends may
// arrive in either order.
//
// Undated items sort first in an ascending grid, and a comparison against NULL would drop them
// from any range that spans them, so the date is read as the empty string, which sorts the same.
func (s *Store) SelectRange(albumID, fromKey, toKey string, selected bool) error {
	_, err := s.db.Exec(`
		INSERT INTO media_selection (media_key, selected)
		SELECT media_key, ? FROM media_items
		WHERE media_key IN (SELECT media_key FROM album_items WHERE album_id = ?)
		  AND (`+gridPlace+`) >= (
				SELECT `+gridPlace+` FROM media_items WHERE media_key IN (?, ?)
				ORDER BY 1, 2 LIMIT 1)
		  AND (`+gridPlace+`) <= (
				SELECT `+gridPlace+` FROM media_items WHERE media_key IN (?, ?)
				ORDER BY 1 DESC, 2 DESC LIMIT 1)
		ON CONFLICT(media_key) DO UPDATE SET selected = excluded.selected`,
		selected, albumID, fromKey, toKey, fromKey, toKey)
	if err != nil {
		return fmt.Errorf("selecting a range of the album: %w", err)
	}
	return nil
}

// gridPlace is an item's position in an ascending grid as a row value, comparable the way the
// grids' ORDER BY compares.
const gridPlace = `COALESCE(captured_at, ''), media_key`

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
