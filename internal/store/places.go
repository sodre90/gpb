package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Area is a box on the map, the way a geocoder describes a place: the southern and northern
// edges in latitude, the western and eastern in longitude.
type Area struct {
	South, North, West, East float64
}

// Where narrows the whole-library grid. The zero value is the whole library.
type Where struct {
	Within *Area
}

func (w Where) clause() (string, []any) {
	if w.Within == nil {
		return "", nil
	}
	return ` WHERE latitude BETWEEN ? AND ? AND longitude BETWEEN ? AND ?`,
		[]any{w.Within.South, w.Within.North, w.Within.West, w.Within.East}
}

// Location is what one file said about where it was taken. Known is false for a file that
// was read and carried nothing.
type Location struct {
	MediaKey  string
	Latitude  float64
	Longitude float64
	Known     bool
}

// Unlocated is the next batch of backed-up files nobody has read a place from yet, oldest
// download first so a sweep works through the library in one order.
func (s *Store) Unlocated(limit int) ([]MediaItem, error) {
	return s.queryItems(`
		SELECT `+itemColumns+` FROM media_items
		WHERE local_path IS NOT NULL AND local_path != '' AND located_at IS NULL
		ORDER BY downloaded_at, media_key LIMIT ?`, limit)
}

// MarkLocated records a batch of readings in one transaction, so that a sweep of a hundred
// thousand files does not cost a hundred thousand commits.
func (s *Store) MarkLocated(locations []Location, at time.Time) error {
	if len(locations) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("recording locations: %w", err)
	}
	defer tx.Rollback()

	statement, err := tx.Prepare(`
		UPDATE media_items SET latitude = ?, longitude = ?, located_at = ? WHERE media_key = ?`)
	if err != nil {
		return fmt.Errorf("recording locations: %w", err)
	}
	defer statement.Close()

	stamp := formatTime(at)
	for _, location := range locations {
		var latitude, longitude sql.NullFloat64
		if location.Known {
			latitude = sql.NullFloat64{Float64: location.Latitude, Valid: true}
			longitude = sql.NullFloat64{Float64: location.Longitude, Valid: true}
		}
		if _, err := statement.Exec(latitude, longitude, stamp, location.MediaKey); err != nil {
			return fmt.Errorf("recording a location: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recording locations: %w", err)
	}
	return nil
}

// LocationProgress is how far the sweep has got: of the files on disk, how many have been
// read for a place, and how many of those had one.
type LocationProgress struct {
	OnDisk  int
	Read    int
	Located int
}

func (p LocationProgress) Finished() bool { return p.Read >= p.OnDisk }

func (s *Store) LocationProgress() (LocationProgress, error) {
	var progress LocationProgress
	err := s.db.QueryRow(`
		SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN located_at IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN latitude IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM media_items WHERE local_path IS NOT NULL AND local_path != ''`).
		Scan(&progress.OnDisk, &progress.Read, &progress.Located)
	if err != nil {
		return LocationProgress{}, fmt.Errorf("counting located files: %w", err)
	}
	return progress, nil
}

// Place is a name as the geocoder answered it. Found is false for a name it did not know,
// which is remembered so that the same wrong name is not asked about on every reload.
type Place struct {
	Query      string
	Name       string
	Area       Area
	Found      bool
	LookedUpAt time.Time
}

// placeKey is how a query is remembered: what was typed, folded so that "fuerteventura" and
// "Fuerteventura " are one lookup.
func placeKey(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(query), " "))
}

func (s *Store) CachedPlace(query string) (Place, bool, error) {
	var place Place
	var south, north, west, east sql.NullFloat64
	var lookedUp string
	err := s.db.QueryRow(`
		SELECT query, name, south, north, west, east, looked_up_at FROM places WHERE query = ?`,
		placeKey(query)).Scan(&place.Query, &place.Name, &south, &north, &west, &east, &lookedUp)
	if err == sql.ErrNoRows {
		return Place{}, false, nil
	}
	if err != nil {
		return Place{}, false, fmt.Errorf("reading a remembered place: %w", err)
	}
	place.Found = south.Valid
	place.Area = Area{South: south.Float64, North: north.Float64, West: west.Float64, East: east.Float64}
	place.LookedUpAt = parseTime(sql.NullString{String: lookedUp, Valid: true})
	return place, true, nil
}

func (s *Store) RememberPlace(place Place) error {
	var south, north, west, east sql.NullFloat64
	if place.Found {
		south = sql.NullFloat64{Float64: place.Area.South, Valid: true}
		north = sql.NullFloat64{Float64: place.Area.North, Valid: true}
		west = sql.NullFloat64{Float64: place.Area.West, Valid: true}
		east = sql.NullFloat64{Float64: place.Area.East, Valid: true}
	}
	_, err := s.db.Exec(`
		INSERT INTO places (query, name, south, north, west, east, looked_up_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(query) DO UPDATE SET name = excluded.name, south = excluded.south,
			north = excluded.north, west = excluded.west, east = excluded.east,
			looked_up_at = excluded.looked_up_at`,
		placeKey(place.Query), place.Name, south, north, west, east, formatTime(place.LookedUpAt))
	if err != nil {
		return fmt.Errorf("remembering a place: %w", err)
	}
	return nil
}
