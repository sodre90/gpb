package store

import (
	"fmt"
	"strings"
)

// Google gives one photograph a different media key in each place it can be reached from — an
// album and the library timeline are two — so a library followed alongside its albums lists the
// same bytes more than once. Measured 2026-09-22: 20,795 photos held under two or more keys,
// 626 GB of a 1.4 TB pool. The rows stay one per key, because each key is how that album or the
// timeline names the photo; it is the file that is shared, as hardlinks.

const backedUpWithAHash = `state = 'done' AND local_path IS NOT NULL AND sha256 IS NOT NULL`

// HeldCopyOf returns an item already backed up with these bytes under a key other than the one
// being downloaded — the earliest downloaded, so every later copy links to the same file.
func (s *Store) HeldCopyOf(sha256, mediaKey string) (MediaItem, bool, error) {
	items, err := s.queryItems(`SELECT `+itemColumns+` FROM media_items
		WHERE `+backedUpWithAHash+` AND sha256 = ? AND media_key != ?
		ORDER BY downloaded_at, media_key LIMIT 1`, sha256, mediaKey)
	switch {
	case err != nil:
		return MediaItem{}, false, fmt.Errorf("looking for a held copy: %w", err)
	case len(items) == 0:
		return MediaItem{}, false, nil
	default:
		return items[0], true, nil
	}
}

// SameBytesGroups lists every set of backed-up items that share their bytes, each set earliest
// download first, so its first item is the one the others should link to.
func (s *Store) SameBytesGroups() ([][]MediaItem, error) {
	items, err := s.queryItems(`SELECT ` + itemColumns + ` FROM media_items
		WHERE ` + backedUpWithAHash + ` AND sha256 IN (
			SELECT sha256 FROM media_items WHERE ` + backedUpWithAHash + `
			GROUP BY sha256 HAVING count(*) > 1)
		ORDER BY sha256, downloaded_at, media_key`)
	if err != nil {
		return nil, fmt.Errorf("finding photos kept more than once: %w", err)
	}

	var groups [][]MediaItem
	for _, item := range items {
		last := len(groups) - 1
		if last >= 0 && strings.EqualFold(groups[last][0].SHA256, item.SHA256) {
			groups[last] = append(groups[last], item)
			continue
		}
		groups = append(groups, []MediaItem{item})
	}
	return groups, nil
}
