package store

import (
	"testing"
)

// A written-off item is paired with the best copy of it, and forgetting it takes its album
// memberships and selection with it, so no row is left pointing at a key that is gone.
func TestAWrittenOffTwinIsPairedWithItsBestCopyAndCanBeForgotten(t *testing.T) {
	store := openTestStore(t)
	albumID := seedFollowedAlbum(t, store, SyncPicked, "gone", "same-name", "same-bytes")
	for key, item := range map[string]MediaItem{
		"gone":       {Filename: "IMAG0003.jpg", SizeBytes: 100, SHA256: "aaaa"},
		"same-name":  {Filename: "IMAG0003.jpg", SizeBytes: 300, SHA256: "bbbb"},
		"same-bytes": {Filename: "IMAG0003.jpg", SizeBytes: 100, SHA256: "aaaa"},
	} {
		item.MediaKey, item.LocalPath = key, "/pool/"+key
		if err := store.MarkDownloaded(item, noon); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}
	if err := store.MarkMissingUpstream("gone", noon); err != nil {
		t.Fatalf("writing off: %v", err)
	}
	if err := store.SelectItem("gone", true); err != nil {
		t.Fatalf("selecting: %v", err)
	}

	twins, err := store.WrittenOffTwins()
	if err != nil {
		t.Fatalf("listing twins: %v", err)
	}
	if len(twins) != 1 || twins[0].WrittenOff.MediaKey != "gone" {
		t.Fatalf("the twins are %+v, want only the written-off item", twins)
	}
	if twins[0].Kept.MediaKey != "same-bytes" || !twins[0].SameBytes {
		t.Errorf("gone is paired with %s (same bytes %v), want the byte-for-byte copy over the larger one",
			twins[0].Kept.MediaKey, twins[0].SameBytes)
	}

	if err := store.Forget("gone"); err != nil {
		t.Fatalf("forgetting: %v", err)
	}
	if _, err := store.Item("gone"); err == nil {
		t.Error("the forgotten item is still there")
	}
	if count, _ := store.AlbumItemCount(albumID); count != 2 {
		t.Errorf("the album still counts %d items, want 2", count)
	}
	if selection, _ := store.SelectionIn(albumID); selection["gone"] {
		t.Error("the forgotten item is still selected")
	}
	if twins, _ := store.WrittenOffTwins(); len(twins) != 0 {
		t.Errorf("%+v are still twins after forgetting", twins)
	}
}
