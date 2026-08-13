package store

import (
	"fmt"
	"os"
)

// BackupTo writes a consistent copy of the database to path, replacing whatever was there.
//
// VACUUM INTO rather than a file copy: the live database is in WAL mode, so its file on disk is
// only half the story and a copy taken while a sync run is writing would be a torn one. VACUUM
// INTO reads through a transaction and writes a complete, already-compacted database.
//
// It refuses to write over an existing file, which is the whole reason for the staging name: the
// alternative is deleting last week's copy before this week's is known to have worked, leaving a
// window with no backup at all in it.
func (s *Store) BackupTo(path string) error {
	staging := path + ".part"
	if err := os.Remove(staging); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing the previous backup attempt: %w", err)
	}

	if _, err := s.db.Exec(`VACUUM INTO ?`, staging); err != nil {
		return fmt.Errorf("copying the database to %s: %w", staging, err)
	}

	if err := os.Rename(staging, path); err != nil {
		return fmt.Errorf("putting the database copy in place: %w", err)
	}
	return nil
}
