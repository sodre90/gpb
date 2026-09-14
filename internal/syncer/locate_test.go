package syncer

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpb/internal/store"
)

// movieAt is the smallest movie that says where it was shot: a moov box holding a ©xyz
// location string, which is what a phone writes.
func movieAt(iso6709 string) []byte {
	box := func(kind string, body ...[]byte) []byte {
		var out bytes.Buffer
		size := 8
		for _, part := range body {
			size += len(part)
		}
		binary.Write(&out, binary.BigEndian, uint32(size))
		out.WriteString(kind)
		for _, part := range body {
			out.Write(part)
		}
		return out.Bytes()
	}
	xyz := box("\xa9xyz", []byte{0, byte(len(iso6709)), 0x15, 0xc7}, []byte(iso6709))
	return append(box("ftyp", []byte("mp42"), make([]byte, 8)), box("moov", box("udta", xyz))...)
}

// A sweep reads each backed-up file once and records what it said; a file it cannot open is
// left for the next sweep rather than recorded as saying nothing.
func TestASweepReadsEachFileOnceAndLeavesTheUnreadableForLater(t *testing.T) {
	pool := t.TempDir()
	db, err := store.Open(filepath.Join(pool, "state.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer db.Close()
	island := filepath.Join(pool, "island.mp4")
	if err := os.WriteFile(island, movieAt("+28.4567-014.0123/"), 0o600); err != nil {
		t.Fatalf("writing the movie: %v", err)
	}
	blank := filepath.Join(pool, "blank.mp4")
	if err := os.WriteFile(blank, movieAt(""), 0o600); err != nil {
		t.Fatalf("writing the movie: %v", err)
	}
	for key, path := range map[string]string{"island": island, "blank": blank, "missing": filepath.Join(pool, "gone.jpg")} {
		if err := db.UpsertItem(store.MediaItem{MediaKey: key, Filename: key}, time.Now()); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
		if err := db.MarkDownloaded(store.MediaItem{MediaKey: key, Filename: key, LocalPath: path}, time.Now()); err != nil {
			t.Fatalf("marking %s downloaded: %v", key, err)
		}
	}

	read, err := LocateBatch(t.Context(), db)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if read != 3 {
		t.Errorf("the sweep read %d files, want all 3 offered", read)
	}
	progress, _ := db.LocationProgress()
	if progress.Read != 2 || progress.Located != 1 {
		t.Errorf("after the sweep the progress is %+v, want 2 read and 1 with a place", progress)
	}
	canaries := store.Where{Within: &store.Area{South: 27.9, North: 28.8, West: -14.6, East: -13.8}}
	items, _ := db.EveryItemPage(canaries, 0, 10)
	if len(items) != 1 || items[0].MediaKey != "island" {
		t.Errorf("the box holds %v, want the island", items)
	}

	waiting, _ := db.Unlocated(10)
	if len(waiting) != 1 || waiting[0].MediaKey != "missing" {
		t.Errorf("%v still wait, want only the file that could not be opened", waiting)
	}
}
