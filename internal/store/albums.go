package store

import (
	"cmp"
	"database/sql"
	"fmt"
	"time"
)

// SyncMode says how much of an album the daemon follows. Albums start at SyncNone: a fresh
// install downloads nothing until the user asks for something, so a first run cannot
// accidentally pull a whole library.
type SyncMode string

const (
	SyncNone   SyncMode = "none"
	SyncAll    SyncMode = "all"
	SyncPicked SyncMode = "picked"
)

func (m SyncMode) valid() bool {
	switch m {
	case SyncNone, SyncAll, SyncPicked:
		return true
	default:
		return false
	}
}

// AlbumKind mirrors gphotos.AlbumKind. The store keeps its own so a schema value is not
// defined by a package it does not depend on.
//
// AlbumOwned is not the same fact as OwnerIsAccount, however much 'own' reads like it. Kind
// records which of Google's surfaces a row came from, because that is what decides the words
// the UI puts in front of it; OwnerIsAccount records who Google says the owner is. The two
// disagree in both directions: an album this account owns and then shared out still arrives in
// the album listing, so it is AlbumOwned with OwnerIsAccount true, while a bundle this account
// shared out is AlbumBundle with OwnerIsAccount true as well. Read kind for what to call a row
// and OwnerIsAccount for whose it is; neither substitutes for the other.
type AlbumKind string

const (
	AlbumOwned   AlbumKind = "own"
	AlbumShared  AlbumKind = "shared"
	AlbumBundle  AlbumKind = "bundle"
	AlbumLibrary AlbumKind = "library"
)

// LibraryID names the row standing for the whole account. It is a word rather than an empty
// string because "" already means "no album covers this item" everywhere else in this store.
const LibraryID = "library"

type Album struct {
	ID        string
	Title     string
	ItemCount int
	SyncMode  SyncMode
	// CreatedAt is Google's date for the album. FirstSeenAt is this store's, and the two are
	// years apart on an old library — the first is when the user made the album, the second
	// merely when this program was pointed at it.
	CreatedAt time.Time
	Kind      AlbumKind
	CoverURL  string
	// OwnerName is who the album or bundle came from, and OwnerIsAccount whether that is the
	// signed-in user. Both are needed: a bundle the user shared themselves and one a friend
	// shared with them are the same row otherwise, and neither has a title to tell them apart.
	OwnerName      string
	OwnerIsAccount bool
	// Favourite is the user's own mark on an album, and the only thing on this row Google has no
	// opinion about. It orders the list they read and the work a run does, in that order.
	Favourite bool
	// Since is the oldest capture date worth walking, and only the library row carries one. The
	// timeline arrives newest first, so it bounds the listing as well as the download: without
	// it, following the library means paging through every photo the account has ever held.
	Since        time.Time
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	LastSyncedAt time.Time
}

// UpsertAlbum records an album seen in a listing. It writes what Google said and nothing the
// user decided, which means it ignores album.SyncMode entirely: a new row is always 'none' and
// an existing row keeps whatever it had. The listing describes Google's state, sync_mode is the
// user's instruction, and a nightly refresh must never overwrite the latter with a default.
// SetAlbumSyncMode is the only way that column changes — filling in the field here does nothing.
func (s *Store) UpsertAlbum(album Album, seenAt time.Time) error {
	seen := formatTime(seenAt)
	// COALESCE, not a plain assignment: a listing that stops carrying the creation date — a
	// drifted field position, an album type that omits it — must not erase the date already
	// recorded. There is no second source for it.
	_, err := s.db.Exec(`
		INSERT INTO albums (id, title, item_count, sync_mode, created_at, kind,
			cover_url, owner_name, owner_is_account, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			item_count = excluded.item_count,
			created_at = COALESCE(excluded.created_at, albums.created_at),
			kind = excluded.kind,
			cover_url = excluded.cover_url,
			owner_name = excluded.owner_name,
			owner_is_account = excluded.owner_is_account,
			last_seen_at = excluded.last_seen_at`,
		album.ID, album.Title, album.ItemCount, string(SyncNone),
		nullableTime(album.CreatedAt), string(cmp.Or(album.Kind, AlbumOwned)),
		album.CoverURL, album.OwnerName, album.OwnerIsAccount, seen, seen)
	if err != nil {
		return fmt.Errorf("upserting album: %w", err)
	}
	return nil
}

func (s *Store) SetAlbumSyncMode(albumID string, mode SyncMode) error {
	if !mode.valid() {
		return fmt.Errorf("unknown sync mode %q", mode)
	}

	result, err := s.db.Exec(`UPDATE albums SET sync_mode = ? WHERE id = ?`, string(mode), albumID)
	if err != nil {
		return fmt.Errorf("setting the sync mode: %w", err)
	}
	return requireOneRow(result, "album", albumID)
}

// SetAlbumFavourite records that this is one of the few albums the user watches. It is the one
// album column Google has no say in, which is why UpsertAlbum leaves it alone: a nightly
// refresh writes everything else on the row from the listing.
func (s *Store) SetAlbumFavourite(albumID string, favourite bool) error {
	result, err := s.db.Exec(`UPDATE albums SET is_favourite = ? WHERE id = ?`, favourite, albumID)
	if err != nil {
		return fmt.Errorf("setting an album favourite: %w", err)
	}
	return requireOneRow(result, "album", albumID)
}

// SetLibrary records both halves of the library's instruction at once, because they are one
// decision: "back up everything" and "back up everything since 2020" are answers to the same
// question, and applying one without the other would start a walk of the whole account.
//
// A zero since clears the bound, which means the entire history.
func (s *Store) SetLibrary(mode SyncMode, since time.Time) error {
	if !mode.valid() {
		return fmt.Errorf("unknown sync mode %q", mode)
	}

	result, err := s.db.Exec(`UPDATE albums SET sync_mode = ?, since_date = ? WHERE id = ?`,
		string(mode), nullableTime(since), LibraryID)
	if err != nil {
		return fmt.Errorf("setting the library instruction: %w", err)
	}
	return requireOneRow(result, "album", LibraryID)
}

func (s *Store) Library() (Album, error) {
	return s.Album(LibraryID)
}

func (s *Store) MarkAlbumSynced(albumID string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE albums SET last_synced_at = ? WHERE id = ?`, formatTime(at), albumID)
	return err
}

func (s *Store) Albums() ([]Album, error) {
	return s.queryAlbums(`SELECT ` + albumColumns + ` FROM albums ORDER BY title, id`)
}

func (s *Store) Album(albumID string) (Album, error) {
	albums, err := s.queryAlbums(`SELECT `+albumColumns+` FROM albums WHERE id = ?`, albumID)
	switch {
	case err != nil:
		return Album{}, err
	case len(albums) == 0:
		return Album{}, fmt.Errorf("no such album: %s", albumID)
	default:
		return albums[0], nil
	}
}

// FollowedAlbums returns the albums a sync run has to visit, favourites first. The library is
// not among them: it is walked by a different listing, and the browsable album view (which asks
// the same question) has nothing to gain from a folder of symlinks to the entire account.
//
// Favourites lead because this is the order a run walks in, and a walk of 181 albums against a
// service asked twice a second is long enough to be interrupted — by a dead session, a full
// disk, or somebody restarting the daemon. What it got through by then should be the albums the
// user said they care about.
func (s *Store) FollowedAlbums() ([]Album, error) {
	return s.queryAlbums(`SELECT `+albumColumns+`
		FROM albums WHERE sync_mode != 'none' AND id != ?
		ORDER BY is_favourite DESC, title, id`, LibraryID)
}

// BackupSet is the whole of what the user has asked to have, counted across the account rather
// than album by album. An item in three followed albums is one photo and one download, so these
// are counts of distinct items — adding up the per-album stats would say three.
//
// Expected is the one figure that is not: it is Google's own per-album count, summed, and it is
// all there is to go on before a sync has walked anything. It is what to show while Known is
// still zero, and it must be shown as the approximation it is.
type BackupSet struct {
	Albums   int
	Library  bool
	Expected int
	Known    int
	Done     int
	Pending  int
	Failed   int
	Bytes    int64
}

func (b BackupSet) Empty() bool { return b.Albums == 0 && !b.Library }

// Walked reports whether anything has listed the contents of the set yet. Until something has,
// every count below Expected is zero — which reads as "there is nothing to do" when it means
// "nobody has looked".
func (b BackupSet) Walked() bool { return b.Known > 0 }

func (s *Store) BackupSet() (BackupSet, error) {
	var set BackupSet

	err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(item_count), 0)
		FROM albums WHERE sync_mode != 'none' AND id != ?`, LibraryID).Scan(&set.Albums, &set.Expected)
	if err != nil {
		return BackupSet{}, fmt.Errorf("counting the albums under backup: %w", err)
	}

	var library int
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM albums WHERE sync_mode != 'none' AND id = ?`, LibraryID).Scan(&library)
	if err != nil {
		return BackupSet{}, fmt.Errorf("asking whether the library is under backup: %w", err)
	}
	set.Library = library > 0

	// Grouped by bytes so a photo held under several keys, which the pool keeps as one hardlinked
	// file, is measured once: the figure is shown as what is on disk. The grouping costs about
	// three quarters again over counting alone — 125 ms to 220 ms at 97,000 items on a laptop,
	// 2026-09-23 — against 305 ms for subtracting the repeats in a second query.
	err = s.db.QueryRow(`
		SELECT COALESCE(SUM(known), 0), COALESCE(SUM(done), 0), COALESCE(SUM(pending), 0),
			COALESCE(SUM(failed), 0), COALESCE(SUM(done_bytes), 0)
		FROM (
			SELECT COUNT(*) AS known,
				SUM(CASE WHEN state = 'done' THEN 1 ELSE 0 END) AS done,
				SUM(CASE WHEN state IN ('discovered', 'queued', 'downloading') THEN 1 ELSE 0 END) AS pending,
				SUM(CASE WHEN state = 'failed' THEN 1 ELSE 0 END) AS failed,
				MAX(CASE WHEN state = 'done' THEN size_bytes END) AS done_bytes
			FROM media_items WHERE `+inTheSyncSet+`
			GROUP BY COALESCE(sha256, media_key))`).
		Scan(&set.Known, &set.Done, &set.Pending, &set.Failed, &set.Bytes)
	if err != nil {
		return BackupSet{}, fmt.Errorf("summarising the backup set: %w", err)
	}
	return set, nil
}

// AlbumStats is one album's progress. The counts cover only items this store has listed,
// which for an album nobody follows is none — nothing walks an album's contents until it is
// followed, so a zero here means "not looked at yet", not "nothing to do".
// Picked is what makes a 'picked' album with nothing picked visible: it downloads nothing
// and looks identical to a followed album whose sync has not run yet.
type AlbumStats struct {
	Known       int
	Done        int
	Pending     int
	Failed      int
	Missing     int
	NeedsReview int
	Picked      int
}

// albumStatsCounts and albumStatsSource are shared by the whole-library summary and the
// single-album one, so the two can never come to disagree about what "done" counts. The
// COALESCE matters only to the single-album query, where an album nobody has walked yet
// aggregates over no rows at all and SUM answers NULL.
const albumStatsCounts = `COUNT(*),
		COALESCE(SUM(CASE WHEN mi.state = 'done' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN mi.state IN ('discovered', 'queued', 'downloading') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN mi.state = 'failed' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN mi.state = 'missing_upstream' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN mi.needs_review = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ms.selected = 1 THEN 1 ELSE 0 END), 0)`

const albumStatsSource = `FROM album_items ai
		JOIN media_items mi ON mi.media_key = ai.media_key
		LEFT JOIN media_selection ms ON ms.media_key = ai.media_key`

// AlbumStats summarises one album, for the pages that show one. Reading a single row out of
// AlbumStatsByID would group over every album's items to answer a question about one.
func (s *Store) AlbumStats(albumID string) (AlbumStats, error) {
	var album AlbumStats
	err := s.db.QueryRow(`SELECT `+albumStatsCounts+` `+albumStatsSource+`
		WHERE ai.album_id = ?`, albumID).Scan(&album.Known, &album.Done, &album.Pending,
		&album.Failed, &album.Missing, &album.NeedsReview, &album.Picked)
	if err != nil {
		return AlbumStats{}, fmt.Errorf("summarising album %s: %w", albumID, err)
	}
	return album, nil
}

// AlbumStatsByID summarises every album in one query. The album list would otherwise issue a
// count per row, and this account has 181 albums.
func (s *Store) AlbumStatsByID() (map[string]AlbumStats, error) {
	rows, err := s.db.Query(`SELECT ai.album_id, ` + albumStatsCounts + ` ` + albumStatsSource + `
		GROUP BY ai.album_id`)
	if err != nil {
		return nil, fmt.Errorf("summarising albums: %w", err)
	}
	defer rows.Close()

	stats := map[string]AlbumStats{}
	for rows.Next() {
		var albumID string
		var album AlbumStats
		if err := rows.Scan(&albumID, &album.Known, &album.Done, &album.Pending,
			&album.Failed, &album.Missing, &album.NeedsReview, &album.Picked); err != nil {
			return nil, fmt.Errorf("scanning an album summary: %w", err)
		}
		stats[albumID] = album
	}
	return stats, rows.Err()
}

// albumColumns keeps the projection and the scan below in one place, so adding a column is
// one edit rather than four that have to agree.
const albumColumns = `id, title, item_count, sync_mode, created_at, kind,
	cover_url, owner_name, owner_is_account, is_favourite, since_date,
	first_seen_at, last_seen_at, last_synced_at`

func (s *Store) queryAlbums(query string, args ...any) ([]Album, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing albums: %w", err)
	}
	defer rows.Close()

	var albums []Album
	for rows.Next() {
		var album Album
		var itemCount sql.NullInt64
		var created, since, firstSeen, lastSeen, lastSynced sql.NullString
		if err := rows.Scan(&album.ID, &album.Title, &itemCount, &album.SyncMode,
			&created, &album.Kind, &album.CoverURL, &album.OwnerName, &album.OwnerIsAccount,
			&album.Favourite, &since, &firstSeen, &lastSeen, &lastSynced); err != nil {
			return nil, fmt.Errorf("scanning an album: %w", err)
		}
		album.ItemCount = int(itemCount.Int64)
		album.CreatedAt = parseTime(created)
		album.Since = parseTime(since)
		album.FirstSeenAt = parseTime(firstSeen)
		album.LastSeenAt = parseTime(lastSeen)
		album.LastSyncedAt = parseTime(lastSynced)
		albums = append(albums, album)
	}
	return albums, rows.Err()
}

func requireOneRow(result sql.Result, kind, id string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("no such %s: %s", kind, id)
	}
	return nil
}
