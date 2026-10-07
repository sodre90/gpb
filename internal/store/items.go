package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// State is where an item sits in the download lifecycle.
//
// StateDownloading exists so a crash is recoverable: an item left in it at startup was
// interrupted mid-write, and the engine requeues it rather than trusting a partial file.
type State string

const (
	StateDiscovered      State = "discovered"
	StateQueued          State = "queued"
	StateDownloading     State = "downloading"
	StateDone            State = "done"
	StateFailed          State = "failed"
	StateMissingUpstream State = "missing_upstream"
)

type MediaItem struct {
	MediaKey     string
	Filename     string
	CapturedAt   time.Time
	SizeBytes    int64
	SHA256       string
	State        State
	FailCount    int
	LastError    string
	LocalPath    string
	FirstSeenAt  time.Time
	DownloadedAt time.Time
	MissingSince time.Time
	NeedsReview  bool
	ThumbnailURL string
	IsVideo      bool
}

// UpsertItem records an item seen in a listing, without disturbing anything the download
// path owns. A re-listing refreshes what Google reports; it must not reset state, fail_count
// or the local path, or every nightly run would re-download the library.
//
// The update is conditional on something having actually changed, which for a library under
// backup is almost never. Writing the row regardless cost a row update for every item a walk
// re-lists — 97,860 a night on the library this was measured on — and SQLite's WAL turned that
// into gigabytes a day of block I/O.
//
// The one state a re-listing does overturn is missing_upstream, because a listing is direct
// evidence against it. Clearing missing_since alone was not enough: Pending looks for the
// live states and DownloadedInSyncSet looks for done, so an item left sitting at
// missing_upstream is in neither set and would never be fetched or verified again however
// many times Google showed it. It returns to done if there is a file to return to, and to
// discovered otherwise — and stops asking for a review it no longer needs.
//
// A missing_since on an item that is not missing_upstream is let through as well. Up to 0.4.2 a
// download that finished while the same run wrote its item off left the item done with the date
// still set. The download path no longer does (see unlessWrittenOff), but the rows it left are
// still in databases, and Reconcile treats any item with a missing_since as already written off
// — so left alone, that photo's real disappearance later would drop it from the album without a
// word.
//
// filename is written on insert and never updated, because a listing does not carry one —
// the caller passes the media key as a placeholder, and the real name arrives with the
// download's Content-Disposition. Copying the placeholder back over it on the next listing
// would rename every already-downloaded item to its media key.
//
// thumbnail_url is the opposite case: the listing is its only source, so a re-listing
// refreshes it. Measured live, the URL is an opaque content id with no signature or expiry,
// but a refresh costs nothing and covers us if that ever stops being true.
func (s *Store) UpsertItem(item MediaItem, firstSeenAt time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO media_items (media_key, filename, captured_at, state,
			first_seen_at, thumbnail_url, is_video)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(media_key) DO UPDATE SET
			captured_at = excluded.captured_at,
			thumbnail_url = excluded.thumbnail_url,
			is_video = excluded.is_video,
			missing_since = NULL,
			needs_review = CASE WHEN state = ? THEN 0 ELSE needs_review END,
			state = CASE
				WHEN state != ? THEN state
				WHEN local_path IS NOT NULL THEN ?
				ELSE ? END
		WHERE captured_at IS NOT excluded.captured_at
			OR thumbnail_url IS NOT excluded.thumbnail_url
			OR is_video != excluded.is_video
			OR state = ?
			OR missing_since IS NOT NULL`,
		item.MediaKey, item.Filename, nullableTime(item.CapturedAt),
		string(StateDiscovered), formatTime(firstSeenAt),
		nullEmpty(item.ThumbnailURL), item.IsVideo,
		string(StateMissingUpstream), string(StateMissingUpstream),
		string(StateDone), string(StateDiscovered), string(StateMissingUpstream))
	if err != nil {
		return fmt.Errorf("upserting media item: %w", err)
	}
	return nil
}

// LinkItemToAlbum records that a listing found an item in an album. A link that is already there
// is left exactly as it is: the walk's own memory of what it saw is what reconciliation reads, so
// re-pressing the same fact into the row ninety thousand times a night said nothing and cost a
// page write each.
func (s *Store) LinkItemToAlbum(albumID, mediaKey string) error {
	_, err := s.db.Exec(`
		INSERT INTO album_items (album_id, media_key) VALUES (?, ?)
		ON CONFLICT(album_id, media_key) DO NOTHING`,
		albumID, mediaKey)
	if err != nil {
		return fmt.Errorf("linking item to album: %w", err)
	}
	return nil
}

// inTheSyncSet is what makes an item one the user asked for: it belongs to an album synced
// 'all', or it is an explicitly selected item of one synced 'picked'.
//
// It is written once because two questions depend on it agreeing with itself — which items a
// run downloads, and how many the summary promises there are. A summary counting a different
// set from the one being fetched would be worse than showing no summary at all.
const inTheSyncSet = `media_key IN (
	SELECT ai.media_key FROM album_items ai
	JOIN albums a ON a.id = ai.album_id
	WHERE a.sync_mode = 'all'
	   OR (a.sync_mode = 'picked' AND ai.media_key IN (
			SELECT media_key FROM media_selection WHERE selected = 1))
)`

// inAFavouriteAlbum is the user's own ordering, applied to items: an item is in it if any album
// holding it is starred. Any, not all — a photo in a favourite album and three others is one the
// user asked for first, and which other albums happen to contain it says nothing about that.
const inAFavouriteAlbum = `media_key IN (
	SELECT ai.media_key FROM album_items ai
	JOIN albums a ON a.id = ai.album_id
	WHERE a.is_favourite = 1
)`

// Pending returns the items a sync run still owes work on: favourite albums first, and within
// each half newest capture first, so a large backlog delivers the photos the user is most
// likely to want before the archive tail.
//
// The favourites go first rather than exclusively first: this is an ordering, not a filter, so
// a run with nothing starred behaves exactly as it did before and a run that is starred to the
// eyeballs still reaches everything else.
func (s *Store) Pending(maxFailures, limit int) ([]MediaItem, error) {
	return s.queryItems(`
		SELECT `+itemColumns+` FROM media_items
		WHERE state IN ('discovered', 'queued', 'downloading', 'failed')
		  AND fail_count < ?
		  AND `+inTheSyncSet+`
		ORDER BY `+inAFavouriteAlbum+` DESC, captured_at DESC, media_key
		LIMIT ?`, maxFailures, limit)
}

// DownloadedInSyncSet lists what this store believes is already on disk and still wanted, so a
// run can check that belief against the filesystem. Items outside the sync set are left out:
// a file the user no longer asks for is not missing, it is simply not being kept up.
func (s *Store) DownloadedInSyncSet() ([]MediaItem, error) {
	return s.queryItems(`
		SELECT ` + itemColumns + ` FROM media_items
		WHERE state = 'done' AND ` + inTheSyncSet + `
		ORDER BY media_key`)
}

// DownloadedFiles lists every item with a file of its own, in or out of the sync set, oldest
// capture first. Verification asks a different question from a sync run: a photograph backed up
// last year and since unfollowed is no longer being kept up, but it is still a file this backup
// put on the disk and still claims is intact.
func (s *Store) DownloadedFiles() ([]MediaItem, error) {
	return s.queryItems(`
		SELECT `+itemColumns+` FROM media_items
		WHERE state = ? AND local_path IS NOT NULL
		ORDER BY captured_at, media_key`, string(StateDone))
}

// DownloadedInAlbum lists the items of one album that are actually on disk, oldest capture
// first. It is what the album link tree is built from: an item with no local file has
// nothing to point at.
func (s *Store) DownloadedInAlbum(albumID string) ([]MediaItem, error) {
	return s.queryItems(`
		SELECT `+itemColumns+` FROM media_items
		WHERE state = ?
		  AND local_path IS NOT NULL
		  AND media_key IN (SELECT media_key FROM album_items WHERE album_id = ?)
		ORDER BY captured_at, media_key`, string(StateDone), albumID)
}

func (s *Store) Item(mediaKey string) (MediaItem, error) {
	items, err := s.queryItems(`SELECT `+itemColumns+` FROM media_items WHERE media_key = ?`, mediaKey)
	switch {
	case err != nil:
		return MediaItem{}, err
	case len(items) == 0:
		return MediaItem{}, fmt.Errorf("no such media item: %s", mediaKey)
	default:
		return items[0], nil
	}
}

// ItemStoredAs finds an item by the name of its file in the pool, which begins with a shortened
// media key — the one handle on an item a person browsing the disk actually has.
func (s *Store) ItemStoredAs(fileName string) (MediaItem, error) {
	suffix := "/" + fileName
	items, err := s.queryItems(`SELECT `+itemColumns+` FROM media_items
		WHERE substr(local_path, -?) = ?`, len(suffix), suffix)
	switch {
	case err != nil:
		return MediaItem{}, err
	case len(items) == 0:
		return MediaItem{}, fmt.Errorf("no backed-up file is called %s", fileName)
	default:
		return items[0], nil
	}
}

// unlessWrittenOff is the state a download-path write leaves an item in. Listing and downloading
// run side by side, so a walk can write an item off between a worker taking it and the worker
// reporting back, and the worker's word is older: overwriting the write-off left a done item still
// dated missing, which the next walk read as already handled and the review queue still counted.
// A file that did arrive is still recorded — a written-off item keeps its file, like any other.
const unlessWrittenOff = `CASE WHEN state = '` + string(StateMissingUpstream) + `' THEN state ELSE ? END`

func (s *Store) SetItemState(mediaKey string, state State) error {
	result, err := s.db.Exec(`UPDATE media_items SET state = `+unlessWrittenOff+` WHERE media_key = ?`,
		string(state), mediaKey)
	if err != nil {
		return fmt.Errorf("setting item state: %w", err)
	}
	return requireOneRow(result, "media item", mediaKey)
}

// MarkDownloaded records a finished file and clears the failure history: the item succeeded,
// so an earlier transient error must not count against a future retry.
func (s *Store) MarkDownloaded(item MediaItem, at time.Time) error {
	result, err := s.db.Exec(`
		UPDATE media_items SET
			state = `+unlessWrittenOff+`, local_path = ?, size_bytes = ?, sha256 = ?,
			filename = ?, downloaded_at = ?, fail_count = 0, last_error = NULL
		WHERE media_key = ?`,
		string(StateDone), item.LocalPath, item.SizeBytes, nullEmpty(item.SHA256),
		item.Filename, formatTime(at), item.MediaKey)
	if err != nil {
		return fmt.Errorf("marking the item downloaded: %w", err)
	}
	return requireOneRow(result, "media item", item.MediaKey)
}

// MarkFailed increments the failure count so Pending can stop offering an item that keeps
// failing, and records the reason for the review queue rather than only the log.
func (s *Store) MarkFailed(mediaKey string, cause error) error {
	result, err := s.db.Exec(`
		UPDATE media_items SET state = `+unlessWrittenOff+`, fail_count = fail_count + 1, last_error = ?
		WHERE media_key = ?`,
		string(StateFailed), cause.Error(), mediaKey)
	if err != nil {
		return fmt.Errorf("marking the item failed: %w", err)
	}
	return requireOneRow(result, "media item", mediaKey)
}

// reviewUnlessACopyRemains is the review flag for an item being written off. Google's
// listing sometimes carries a photo under a second key for a few days and then drops it — a
// library of 96,000, with nothing uploaded to it, had 300 written off in its first month, and
// 145 of those were the same photo as one still there and still backed up: 85 byte for byte,
// 60 as a smaller file with the same name and the same capture second. Losing one of two
// copies is not a loss, so neither is asked about. The copy that stays has to be at least as
// large: the day the larger one goes and the smaller survives is a question, and stays one.
const reviewUnlessACopyRemains = `CASE WHEN
	EXISTS (SELECT 1 FROM media_items copy WHERE ` + sameBytesCopy + `)
	OR EXISTS (SELECT 1 FROM media_items copy WHERE ` + sameNameCopy + `)
	THEN 0 ELSE 1 END`

// sameBytesCopy and sameNameCopy are the two ways a backed-up row, copy, holds the same
// photograph as media_items: the same bytes, or the same name taken in the same second and at
// least as large. They are the one rule in one place, so the review queue and the duplicates
// command cannot disagree about what a copy is — and they are two halves rather than one OR
// because each half has an index of its own (0016), and SQLite would not use either behind an
// OR: one join took 9 s over a library of 97,000, and 2 ms as two.
const (
	sameBytesCopy = `copy.sha256 = media_items.sha256 AND media_items.sha256 IS NOT NULL
	  AND copy.media_key != media_items.media_key AND copy.state = 'done'`
	sameNameCopy = `copy.filename = media_items.filename AND copy.captured_at = media_items.captured_at
	  AND copy.size_bytes >= media_items.size_bytes AND media_items.filename != ''
	  AND copy.media_key != media_items.media_key AND copy.state = 'done'`
)

// MarkMissingUpstream flags an item that has vanished from Google. It never deletes: a
// backup whose contents disappear because the source did is not a backup, so the local file
// stays and the item surfaces for review.
//
// The missing_since guard makes this idempotent: a second run must not restamp the date on
// which an item was first noticed gone.
func (s *Store) MarkMissingUpstream(mediaKey string, at time.Time) error {
	if _, err := s.db.Exec(`
		UPDATE media_items SET state = ?, missing_since = ?, needs_review = `+reviewUnlessACopyRemains+`
		WHERE media_key = ? AND missing_since IS NULL`,
		string(StateMissingUpstream), formatTime(at), mediaKey); err != nil {
		return fmt.Errorf("marking the item missing: %w", err)
	}
	return nil
}

// Departures is what a completed album listing found had left it, split by how much it
// proves. GoneFromGoogle counts a subset of LeftTheAlbum, and is usually zero: a photo taken
// out of one album is still in the library and in every other album that holds it. Copies
// counts the subset of those whose bytes are still held under another key, which the review
// queue is not asked about.
type Departures struct {
	LeftTheAlbum   int
	GoneFromGoogle int
	Copies         int
}

// Reconcile records what a listing implies about the members it no longer holds. The caller
// passes the members the walk was obliged to see and did not — a set the walk already has, being
// the membership that was there when it started, less everything it went past. Only that set is
// passed, so a walk that vouches for part of an album vouches with part of its departures and
// this has no window of its own to apply.
//
// Two different facts come out of that one observation, and reading them as a single fact is
// how a photo removed from one album came to be recorded as gone from Google everywhere:
// having left this album is certain, while having gone from Google is only true if nothing
// else still lists it. The second question is asked of every other album's membership, which
// is the only evidence there is — a photo taken out of an album is still in the library, and
// the library is an album like any other for this purpose. Erring that way keeps backing up a
// photo Google has deleted, which costs disk; erring the other way writes off a photo that is
// still there, which costs the backup.
//
// The membership row is deleted rather than left to age, so the album grid shows the album as
// it is now and a second run over the same absence finds nothing left to do. An item written
// off keeps its membership instead: the review queue groups by the album an item was found in,
// and for an item nothing else lists, this album is the only place it can be asked about — the
// one thing the queue exists to report would otherwise be the one thing it could never show.
func (s *Store) Reconcile(albumID string, departed []string, listedAt time.Time) (Departures, error) {
	if len(departed) == 0 {
		return Departures{}, nil
	}

	listed := formatTime(listedAt)
	tx, err := s.db.Begin()
	if err != nil {
		return Departures{}, fmt.Errorf("reconciling an album: %w", err)
	}
	defer tx.Rollback()

	left, writtenOff := 0, 0
	for chunk := range slices.Chunk(departed, departureChunk) {
		names, keys := keyList(chunk)

		goneArgs := append([]any{string(StateMissingUpstream), listed}, keys...)
		gone, err := tx.Exec(`
			UPDATE media_items SET state = ?, missing_since = ?, needs_review = `+reviewUnlessACopyRemains+`
			WHERE missing_since IS NULL
			  AND media_key IN (`+names+`)
			  AND NOT EXISTS (
					SELECT 1 FROM album_items elsewhere
					WHERE elsewhere.media_key = media_items.media_key
					  AND elsewhere.album_id != ?)`,
			append(goneArgs, albumID)...)
		if err != nil {
			return Departures{}, fmt.Errorf("marking vanished items: %w", err)
		}
		writtenOff += rowsAffected(gone)

		links := append([]any{albumID}, keys...)
		removed, err := tx.Exec(`
			DELETE FROM album_items WHERE album_id = ? AND media_key IN (`+names+`)
			  AND media_key NOT IN (SELECT media_key FROM media_items WHERE state = ?)`,
			append(links, string(StateMissingUpstream))...)
		if err != nil {
			return Departures{}, fmt.Errorf("removing items from an album: %w", err)
		}
		left += rowsAffected(removed)
	}

	var copies int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM media_items WHERE missing_since = ? AND needs_review = 0`, listed).Scan(&copies); err != nil {
		return Departures{}, fmt.Errorf("counting the copies written off: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Departures{}, fmt.Errorf("reconciling an album: %w", err)
	}
	return Departures{LeftTheAlbum: left + writtenOff, GoneFromGoogle: writtenOff, Copies: copies}, nil
}

// departureChunk is how many keys one statement names. An album emptied upstream arrives as one
// large list, and naming them all at once would run into SQLite's bound-parameter limit long
// before a backup had a library that large.
const departureChunk = 500

// keyList is an IN clause of the given length and the arguments that fill it.
func keyList(keys []string) (names string, args []any) {
	placeholders := make([]string, len(keys))
	args = make([]any, len(keys))
	for at, key := range keys {
		placeholders[at] = "?"
		args[at] = key
	}
	return strings.Join(placeholders, ", "), args
}

func rowsAffected(result sql.Result) int {
	count, err := result.RowsAffected()
	if err != nil {
		return 0
	}
	return int(count)
}

// AlbumMembers is an album's contents as they stand, each item carrying the capture date the
// timeline is ordered by. A walk reads it before it starts, because both questions it has to
// answer are about the difference: an item that was here and was not walked past has left, and
// an item that was not here and was walked past has arrived.
//
// An item Google never dated comes back zero, which a walk that stopped at a date bound reads as
// "no known place in the timeline" rather than as old — an item with no place in the order cannot
// be shown to have been passed over.
func (s *Store) AlbumMembers(albumID string) (map[string]time.Time, error) {
	rows, err := s.db.Query(`
		SELECT ai.media_key, mi.captured_at FROM album_items ai
		LEFT JOIN media_items mi ON mi.media_key = ai.media_key
		WHERE ai.album_id = ?`, albumID)
	if err != nil {
		return nil, fmt.Errorf("reading album membership: %w", err)
	}
	defer rows.Close()

	members := map[string]time.Time{}
	for rows.Next() {
		var key string
		var captured sql.NullString
		if err := rows.Scan(&key, &captured); err != nil {
			return nil, err
		}
		members[key] = parseTime(captured)
	}
	return members, rows.Err()
}

// AlbumForItem names an album that currently justifies downloading this item. The download
// RPC needs one for its permission check, and any followed album containing the item will
// do; MIN keeps the choice stable between runs. An empty string means the item is no longer
// covered by anything the user follows, which is a skip rather than a failure.
func (s *Store) AlbumForItem(mediaKey string) (string, error) {
	var albumID sql.NullString
	err := s.db.QueryRow(`
		SELECT MIN(ai.album_id) FROM album_items ai
		JOIN albums a ON a.id = ai.album_id
		WHERE ai.media_key = ?
		  AND (a.sync_mode = 'all'
		   OR (a.sync_mode = 'picked' AND ai.media_key IN (
				SELECT media_key FROM media_selection WHERE selected = 1)))`,
		mediaKey).Scan(&albumID)
	if err != nil {
		return "", fmt.Errorf("finding an album for the item: %w", err)
	}
	return albumID.String, nil
}

func (s *Store) SelectItem(mediaKey string, selected bool) error {
	_, err := s.db.Exec(`
		INSERT INTO media_selection (media_key, selected) VALUES (?, ?)
		ON CONFLICT(media_key) DO UPDATE SET selected = excluded.selected`,
		mediaKey, selected)
	if err != nil {
		return fmt.Errorf("selecting item: %w", err)
	}
	return nil
}

// Counts summarises the library for the status page without pulling every row into memory.
func (s *Store) Counts() (map[State]int, error) {
	rows, err := s.db.Query(`SELECT state, COUNT(*) FROM media_items GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("counting items: %w", err)
	}
	defer rows.Close()

	counts := map[State]int{}
	for rows.Next() {
		var state State
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		counts[state] = count
	}
	return counts, rows.Err()
}

const itemColumns = `media_key, filename, captured_at, size_bytes, sha256, state,
	fail_count, last_error, local_path, first_seen_at, downloaded_at,
	missing_since, needs_review, thumbnail_url, is_video`

func (s *Store) queryItems(query string, args ...any) ([]MediaItem, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying media items: %w", err)
	}
	defer rows.Close()

	var items []MediaItem
	for rows.Next() {
		var item MediaItem
		var size sql.NullInt64
		var sha, lastError, localPath, thumbnailURL sql.NullString
		var capturedAt, firstSeen, downloadedAt, missingSince sql.NullString

		if err := rows.Scan(&item.MediaKey, &item.Filename, &capturedAt, &size,
			&sha, &item.State, &item.FailCount, &lastError, &localPath, &firstSeen,
			&downloadedAt, &missingSince, &item.NeedsReview, &thumbnailURL,
			&item.IsVideo); err != nil {
			return nil, fmt.Errorf("scanning a media item: %w", err)
		}

		item.SizeBytes = size.Int64
		item.SHA256 = sha.String
		item.LastError = lastError.String
		item.LocalPath = localPath.String
		item.ThumbnailURL = thumbnailURL.String
		item.CapturedAt = parseTime(capturedAt)
		item.FirstSeenAt = parseTime(firstSeen)
		item.DownloadedAt = parseTime(downloadedAt)
		item.MissingSince = parseTime(missingSince)
		items = append(items, item)
	}
	return items, rows.Err()
}

func nullEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
