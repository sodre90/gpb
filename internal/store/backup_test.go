package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackupToIsReadableAsADatabase(t *testing.T) {
	source := openTestStore(t)
	if err := source.UpsertAlbum(Album{ID: "a", Title: "Holiday"}, noon); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	path := filepath.Join(t.TempDir(), "state.backup.db")
	if err := source.BackupTo(path); err != nil {
		t.Fatalf("backing up: %v", err)
	}

	copied, err := Open(path)
	if err != nil {
		t.Fatalf("opening the copy: %v", err)
	}
	defer copied.Close()

	album, err := copied.Album("a")
	if err != nil {
		t.Fatalf("reading the album out of the copy: %v", err)
	}
	if album.Title != "Holiday" {
		t.Fatalf("the copy holds %+v", album)
	}
}

// The second week's copy has to replace the first week's, and VACUUM INTO refuses an existing
// file — so a backup that only worked once would be a backup that silently stopped.
func TestBackupToReplacesTheOneBefore(t *testing.T) {
	store := openTestStore(t)
	path := filepath.Join(t.TempDir(), "state.backup.db")

	if err := store.BackupTo(path); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if err := store.UpsertAlbum(Album{ID: "later", Title: "Added afterwards"}, noon); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := store.BackupTo(path); err != nil {
		t.Fatalf("second backup: %v", err)
	}

	copied, err := Open(path)
	if err != nil {
		t.Fatalf("opening the copy: %v", err)
	}
	defer copied.Close()

	if _, err := copied.Album("later"); err != nil {
		t.Fatalf("the second copy is the first one over again: %v", err)
	}
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Fatalf("the staging file was left behind: %v", err)
	}
}

// A backup interrupted halfway leaves a .part behind, and the next attempt must not be blocked
// by it for ever.
func TestBackupToClearsAnAbandonedAttempt(t *testing.T) {
	store := openTestStore(t)
	path := filepath.Join(t.TempDir(), "state.backup.db")

	if err := os.WriteFile(path+".part", []byte("half a database"), 0o600); err != nil {
		t.Fatalf("planting the abandoned attempt: %v", err)
	}
	if err := store.BackupTo(path); err != nil {
		t.Fatalf("backing up over an abandoned attempt: %v", err)
	}

	copied, err := Open(path)
	if err != nil {
		t.Fatalf("opening the copy: %v", err)
	}
	copied.Close()
}
