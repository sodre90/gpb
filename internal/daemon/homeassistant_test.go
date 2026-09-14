package daemon

import (
	"testing"
	"time"

	"gpb/internal/store"
)

// The bridge asks every five seconds and the count walks every item, so the daemon answers
// from memory between recounts — sooner while a run is changing the numbers.
func TestTheDashboardCountsAreRememberedBetweenRecounts(t *testing.T) {
	daemon := scheduledDaemon(t, "03:30")
	now := time.Now()
	seed := func(key string) {
		t.Helper()
		if err := daemon.store.UpsertAlbum(store.Album{ID: "a", Title: "A"}, now); err != nil {
			t.Fatal(err)
		}
		if err := daemon.store.SetAlbumSyncMode("a", store.SyncAll); err != nil {
			t.Fatal(err)
		}
		if err := daemon.store.UpsertItem(store.MediaItem{MediaKey: key, Filename: key + ".jpg"}, now); err != nil {
			t.Fatal(err)
		}
		if err := daemon.store.LinkItemToAlbum("a", key, now); err != nil {
			t.Fatal(err)
		}
	}

	seed("one")
	first, err := daemon.countBackupSet(false, now)
	if err != nil || first.Known != 1 {
		t.Fatalf("the first count is %+v (%v), want 1 known", first, err)
	}

	seed("two")
	if again, _ := daemon.countBackupSet(false, now.Add(time.Minute)); again.Known != 1 {
		t.Errorf("a minute later, idle, the count is %d, want the remembered 1", again.Known)
	}
	if running, _ := daemon.countBackupSet(true, now.Add(time.Minute)); running.Known != 2 {
		t.Errorf("a minute later, with a run going, the count is %d, want a fresh 2", running.Known)
	}
	if later, _ := daemon.countBackupSet(false, now.Add(time.Minute+recountWhileIdle)); later.Known != 2 {
		t.Errorf("after the idle interval the count is %d, want a fresh 2", later.Known)
	}
}
