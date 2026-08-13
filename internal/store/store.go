// Package store is the daemon's crash-safe record of what exists upstream, what has been
// downloaded and what still owes work. It is the only component that outlives a process,
// so every write it accepts has to leave the database in a state a restart can resume from.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// busyTimeout gives a blocked writer time to wait out the other one instead of failing the
// sync run. WAL keeps readers off the writer's back, so contention here is brief by design.
const busyTimeout = 10 * time.Second

type Store struct {
	db *sql.DB
}

// Open prepares the database at path, applying any migrations it has not yet seen.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	// SQLite serialises writers anyway, and a single connection removes the class of
	// SQLITE_BUSY failures that come from this process competing with itself.
	db.SetMaxOpenConns(1)

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func dsn(path string) string {
	pragmas := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=foreign_keys(1)",
		"_pragma=busy_timeout(" + strconv.Itoa(int(busyTimeout.Milliseconds())) + ")",
	}
	return path + "?" + strings.Join(pragmas, "&")
}

func (s *Store) Close() error {
	return s.db.Close()
}

// migrate applies every embedded migration whose number exceeds PRAGMA user_version, in
// order, each inside its own transaction. A crash mid-run therefore leaves the schema at a
// version that matches what is actually in the file.
func (s *Store) migrate() error {
	applied, err := s.userVersion()
	if err != nil {
		return err
	}

	pending, err := pendingMigrations(applied)
	if err != nil {
		return err
	}

	for _, migration := range pending {
		if err := s.apply(migration); err != nil {
			return fmt.Errorf("applying migration %s: %w", migration.name, err)
		}
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func pendingMigrations(applied int) ([]migration, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}

	var pending []migration
	for _, entry := range entries {
		version, err := versionOf(entry.Name())
		if err != nil {
			return nil, err
		}
		if version <= applied {
			continue
		}

		body, err := fs.ReadFile(migrations, filepath.Join("migrations", entry.Name()))
		if err != nil {
			return nil, err
		}
		pending = append(pending, migration{version: version, name: entry.Name(), sql: string(body)})
	}

	sort.Slice(pending, func(i, j int) bool { return pending[i].version < pending[j].version })
	return pending, nil
}

// versionOf reads the leading number of a "0001_init.sql" filename. A file that does not
// follow the convention is a build-time mistake, and failing here is better than silently
// skipping a schema change.
func versionOf(name string) (int, error) {
	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0, fmt.Errorf("migration %q has no version prefix", name)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("migration %q has an unparseable version: %w", name, err)
	}
	return version, nil
}

// apply runs one migration and stamps the new version in the same transaction, so the two
// can never disagree. user_version takes no placeholder, hence the formatted literal — the
// value is an integer parsed from an embedded filename, never external input.
func (s *Store) apply(m migration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(m.sql); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) userVersion() (int, error) {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("reading the schema version: %w", err)
	}
	return version, nil
}

// timestampLayout is RFC3339 in UTC with a fixed-width fractional part. The width matters:
// SQL compares these as strings, and time.RFC3339Nano trims trailing zeros, so
// "12:00:00Z" would sort after "12:00:00.5Z" and a vanished item could slip past
// ItemsMissingFrom undetected. Padding every value to nine digits makes lexical order and
// chronological order the same thing.
const timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(at time.Time) string {
	return at.UTC().Format(timestampLayout)
}

func nullableTime(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return formatTime(at)
}

func parseTime(value sql.NullString) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return time.Time{}
	}
	return at
}
