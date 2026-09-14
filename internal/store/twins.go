package store

import (
	"fmt"
)

// Twin is a written-off file whose photograph is still backed up under another key. SameBytes
// says which of the two rules made it one: the same hash, or the same name and capture second
// with the kept copy at least as large.
type Twin struct {
	WrittenOff MediaItem
	Kept       MediaItem
	SameBytes  bool
}

// WrittenOffTwins lists every written-off item that still has a file, paired with the
// backed-up copy of it, oldest capture first. An item with more than one copy is paired with
// the best of them: one with the same bytes before one with the same name, and the largest
// after that.
func (s *Store) WrittenOffTwins() ([]Twin, error) {
	rows, err := s.db.Query(`
		SELECT media_items.media_key, copy.media_key,
		       COALESCE(copy.sha256 = media_items.sha256, 0)
		FROM media_items JOIN media_items copy ON `+copyStillHeld+`
		WHERE media_items.state = ? AND media_items.local_path IS NOT NULL
		  AND copy.local_path IS NOT NULL
		ORDER BY media_items.captured_at, media_items.media_key,
		         COALESCE(copy.sha256 = media_items.sha256, 0) DESC, copy.size_bytes DESC`,
		string(StateMissingUpstream))
	if err != nil {
		return nil, fmt.Errorf("finding written-off copies: %w", err)
	}
	defer rows.Close()

	type pairing struct {
		writtenOff, kept string
		sameBytes        bool
	}
	var pairings []pairing
	for rows.Next() {
		var pair pairing
		if err := rows.Scan(&pair.writtenOff, &pair.kept, &pair.sameBytes); err != nil {
			return nil, fmt.Errorf("scanning a written-off copy: %w", err)
		}
		if len(pairings) > 0 && pairings[len(pairings)-1].writtenOff == pair.writtenOff {
			continue
		}
		pairings = append(pairings, pair)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finding written-off copies: %w", err)
	}

	var twins []Twin
	for _, pair := range pairings {
		writtenOff, err := s.Item(pair.writtenOff)
		if err != nil {
			return nil, err
		}
		kept, err := s.Item(pair.kept)
		if err != nil {
			return nil, err
		}
		twins = append(twins, Twin{WrittenOff: writtenOff, Kept: kept, SameBytes: pair.sameBytes})
	}
	return twins, nil
}

// Forget removes an item's row, and the album memberships and selection that point at it,
// as one. It is the only way a row leaves media_items, and the duplicates command is the only
// caller: the photograph the row recorded is still recorded under the key of the copy that
// stays, so nothing about the library is forgotten with it.
func (s *Store) Forget(mediaKey string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("forgetting an item: %w", err)
	}
	defer tx.Rollback()

	for _, table := range []string{"album_items", "media_selection", "media_items"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE media_key = ?`, mediaKey); err != nil {
			return fmt.Errorf("forgetting an item: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("forgetting an item: %w", err)
	}
	return nil
}
