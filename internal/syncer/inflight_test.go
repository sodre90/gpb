package syncer

import (
	"io"
	"testing"

	"gpb/internal/store"
)

func TestABoardCountsBytesAsTheyAreWrittenAndTakesTheNameAndSizeFromTheServer(t *testing.T) {
	board := newInFlightBoard()
	item := board.start(store.MediaItem{MediaKey: "key-a"}, "album-1")

	writer := item.streaming(0, io.Discard)
	writer.Announce("IMG_0001.HEIC", 400)
	if _, err := writer.Write(make([]byte, 100)); err != nil {
		t.Fatalf("writing through the meter: %v", err)
	}

	flights := board.snapshot()
	if len(flights) != 1 {
		t.Fatalf("the board holds %d downloads, want 1", len(flights))
	}
	want := InFlight{MediaKey: "key-a", Filename: "IMG_0001.HEIC", AlbumID: "album-1", Written: 100, Total: 400}
	if flights[0] != want {
		t.Errorf("the board says %+v, want %+v", flights[0], want)
	}
}

// A retry, or a resume the server refused, starts the bytes again. Counting on from where the
// failed attempt stopped would take the file past its own size.
func TestAnAttemptStartsTheCountFromWhatIsAlreadyOnDisk(t *testing.T) {
	board := newInFlightBoard()
	item := board.start(store.MediaItem{MediaKey: "key-a"}, "album-1")

	first := item.streaming(0, io.Discard)
	first.Announce("IMG_0001.HEIC", 400)
	first.Write(make([]byte, 250))

	second := item.streaming(250, io.Discard)
	second.Write(make([]byte, 50))

	flight := board.snapshot()[0]
	if flight.Written != 300 {
		t.Errorf("a resumed download has written %d bytes, want 300", flight.Written)
	}
	// The second attempt has not been told a length yet, and the first one's covered a body this
	// attempt is not fetching.
	if flight.Total != 0 {
		t.Errorf("a resumed download claims a total of %d before the server has said", flight.Total)
	}
}

// Google's listing carries no filename: an item that has never been downloaded has no name to
// show, and the response that brings the bytes is where one first appears.
func TestAnItemWithNoNameTakesTheOneTheServerGivesIt(t *testing.T) {
	board := newInFlightBoard()
	item := board.start(store.MediaItem{MediaKey: "key-a"}, "album-1")

	if named := board.snapshot()[0].Filename; named != "" {
		t.Errorf("an item with no name of its own is called %q", named)
	}

	item.streaming(0, io.Discard).Announce("IMG_0001.HEIC", 400)
	if named := board.snapshot()[0].Filename; named != "IMG_0001.HEIC" {
		t.Errorf("the name the server gave was not taken up: %q", named)
	}
}

// A resume asks for the tail of a file, and a host that answers without a Content-Disposition
// must not take the name off a download that already had one.
func TestAServerThatSendsNoNameLeavesTheOneAlreadyKnown(t *testing.T) {
	board := newInFlightBoard()
	item := board.start(store.MediaItem{MediaKey: "key-a"}, "album-1")

	item.streaming(0, io.Discard).Announce("IMG_0001.HEIC", 400)
	item.streaming(250, io.Discard).Announce("", 400)

	if named := board.snapshot()[0].Filename; named != "IMG_0001.HEIC" {
		t.Errorf("a resumed download lost its name and is now %q", named)
	}
}

func TestAFinishedDownloadComesOffTheBoard(t *testing.T) {
	board := newInFlightBoard()
	item := board.start(store.MediaItem{MediaKey: "key-a"}, "album-1")
	item.done()

	if flights := board.snapshot(); len(flights) != 0 {
		t.Errorf("the board still holds %+v after the download finished", flights)
	}
}

// The card renders the rows in the order the board hands them over, so an order that moved
// would shuffle the rows under the reader's eyes every second.
func TestTheBoardKeepsDownloadsInTheOrderTheyStarted(t *testing.T) {
	board := newInFlightBoard()
	for _, key := range []string{"key-a", "key-b", "key-c"} {
		board.start(store.MediaItem{MediaKey: key}, "album-1")
	}

	for _, flights := range [][]InFlight{board.snapshot(), board.snapshot()} {
		for at, want := range []string{"key-a", "key-b", "key-c"} {
			if flights[at].MediaKey != want {
				t.Errorf("row %d is %s, want %s", at, flights[at].MediaKey, want)
			}
		}
	}
}

// This is what the Now card is for: a run that reports only its counts can say how far it has
// got but never what it is doing.
func TestARunSaysWhatItIsDownloadingWhileItDownloadsIt(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "photo a bytes"})

	var duringDownload []InFlight
	h.source.onDownload = func() {
		duringDownload = h.syncer.Progress().Items
	}

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	if len(duringDownload) != 1 {
		t.Fatalf("the run reported %d downloads in flight, want 1", len(duringDownload))
	}
	if duringDownload[0].MediaKey != "key-a" || duringDownload[0].AlbumID != "album-1" {
		t.Errorf("the run is downloading %+v, want key-a from album-1", duringDownload[0])
	}
	// What the response said reaches the board only if the writer the syncer streams through is
	// the one the download is handed, which is the whole mechanism in one line.
	if duringDownload[0].Filename != "key-a.jpg" || duringDownload[0].Total != int64(len("photo a bytes")) {
		t.Errorf("the run is downloading %+v, without what the response said about it", duringDownload[0])
	}
	if left := h.syncer.Progress().Items; len(left) != 0 {
		t.Errorf("the finished run still claims to be downloading %+v", left)
	}
}

// The Now card's run bar has no denominator without this, and the count has to come from the
// store: what a run owes is not known when it starts, because the listing is still finding it.
func TestARunSaysHowManyItemsItOwes(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "photo a", "key-b": "photo b"})

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	if owed := h.syncer.Progress().Owed; owed != 2 {
		t.Errorf("the run owed %d items, want 2", owed)
	}
}
