package syncer

import (
	"cmp"
	"io"
	"slices"
	"sync"

	"gpb/internal/gphotos"
	"gpb/internal/store"
)

// InFlight is one item the run is fetching at this moment. A run that reports only its finished
// counts can say how much is left but never what it is doing, and on a backup measured in days
// what it is doing is the part a person watching wants to see.
type InFlight struct {
	MediaKey string
	Filename string
	AlbumID  string
	Written  int64
	// Total is the whole length of the file, including any part of it already on disk, and is
	// zero until the content host has said what that is.
	Total int64
}

// inFlightBoard is what the workers write to while they download and the web server reads from
// while it renders. One lock covers the whole board rather than one per item: three workers and
// a reader once a second contend for it about as often as they contend for the disk.
type inFlightBoard struct {
	mu      sync.Mutex
	started int64
	items   map[string]*inFlightItem
}

func newInFlightBoard() *inFlightBoard {
	return &inFlightBoard{items: map[string]*inFlightItem{}}
}

// start puts an item on the board for as long as a worker is busy with it, retries and backoff
// included: an item that is being waited on before its next attempt is still what that worker is
// doing, and dropping it off the board would leave a gap the card could not explain.
func (b *inFlightBoard) start(item store.MediaItem, albumID string) *inFlightItem {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.started++
	entry := &inFlightItem{
		board:    b,
		order:    b.started,
		mediaKey: item.MediaKey,
		filename: item.Filename,
		albumID:  albumID,
	}
	b.items[item.MediaKey] = entry
	return entry
}

// snapshot is ordered by when each download started, so the rows on the card keep their places
// instead of shuffling every time it is rendered.
func (b *inFlightBoard) snapshot() []InFlight {
	b.mu.Lock()
	defer b.mu.Unlock()

	entries := make([]*inFlightItem, 0, len(b.items))
	for _, entry := range b.items {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b *inFlightItem) int { return cmp.Compare(a.order, b.order) })

	flights := make([]InFlight, 0, len(entries))
	for _, entry := range entries {
		flights = append(flights, InFlight{
			MediaKey: entry.mediaKey,
			Filename: entry.filename,
			AlbumID:  entry.albumID,
			Written:  entry.written,
			Total:    entry.total,
		})
	}
	return flights
}

// inFlightItem is one row of the board. Its counts are guarded by the board's lock, which is
// held by every method here.
type inFlightItem struct {
	board    *inFlightBoard
	order    int64
	mediaKey string
	filename string
	albumID  string
	written  int64
	total    int64
}

// streaming hands back the writer the file's bytes travel through. It is called once per attempt
// at the bytes — a retry, or a resume the server refused — and each attempt starts the count
// again from whatever survived on disk.
func (i *inFlightItem) streaming(offset int64, inner io.Writer) *meter {
	i.board.mu.Lock()
	defer i.board.mu.Unlock()

	i.written, i.total = offset, 0
	return &meter{item: i, inner: inner}
}

func (i *inFlightItem) done() {
	i.board.mu.Lock()
	defer i.board.mu.Unlock()

	delete(i.board.items, i.mediaKey)
}

func (i *inFlightItem) advance(written int64) {
	i.board.mu.Lock()
	defer i.board.mu.Unlock()

	i.written += written
}

// announced takes what the response said about the file. The name is kept only if there is one:
// a host that sends no Content-Disposition must not blank out the name the listing gave.
func (i *inFlightItem) announced(filename string, total int64) {
	i.board.mu.Lock()
	defer i.board.mu.Unlock()

	i.total = total
	if filename != "" {
		i.filename = filename
	}
}

// meter counts the bytes of one download where they actually land, on their way to the file and
// the hash.
type meter struct {
	item  *inFlightItem
	inner io.Writer
}

func (m *meter) Write(p []byte) (int, error) {
	written, err := m.inner.Write(p)
	m.item.advance(int64(written))
	return written, err
}

// Announce is the optional upgrade gphotos.Fetch looks for on the writer it streams into. It is
// found by type assertion, so nothing would fail to compile if the two drifted apart — the names
// and percentages would simply stop appearing. Hence the declaration below.
func (m *meter) Announce(filename string, total int64) {
	m.item.announced(filename, total)
}

var _ gphotos.Announcer = (*meter)(nil)
