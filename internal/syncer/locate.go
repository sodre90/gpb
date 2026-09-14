package syncer

import (
	"context"
	"log"
	"os"
	"time"

	"gpb/internal/geo"
	"gpb/internal/store"
)

// Where a photo was taken comes from the file itself: Google's listing carries no place, and
// the originals in the pool carry their GPS tags untouched. A file is read for it once — when
// it lands, for new downloads, and by a sweep for the library that was there before this was.

// locateBatchSize is how many files a sweep reads between commits. The reading is what takes
// the time — on a cold spinning disk around a tenth of a second a file — so a batch is a
// minute's work and the count on the page moves that often.
const locateBatchSize = 500

// LocateBatch reads the place out of the next files nobody has asked yet and records what it
// found. It returns how many it recorded; zero means the sweep has nothing left it can do
// now. A file that cannot be opened is left for a later sweep rather than recorded as having
// no place — the mount may be back by then — and it is the recorded count, not the offered
// one, that the caller loops on, or a single such file would keep it going round for ever.
func LocateBatch(ctx context.Context, db *store.Store) (int, error) {
	waiting, err := db.Unlocated(locateBatchSize)
	if err != nil {
		return 0, err
	}
	var located []store.Location
	unreadable := 0
	for _, item := range waiting {
		if ctx.Err() != nil {
			break
		}
		location, err := locate(item)
		if err != nil {
			unreadable++
			continue
		}
		located = append(located, location)
	}
	if unreadable > 0 {
		log.Printf("locator: %d of %d files could not be read and will be tried again later", unreadable, len(waiting))
	}
	if err := db.MarkLocated(located, time.Now()); err != nil {
		return 0, err
	}
	return len(located), nil
}

// LocateOne reads the place out of a file that has just landed.
func LocateOne(db *store.Store, item store.MediaItem) error {
	location, err := locate(item)
	if err != nil {
		log.Printf("locator: reading a file that just landed for its place: %v", err)
		return nil
	}
	return db.MarkLocated([]store.Location{location}, time.Now())
}

func locate(item store.MediaItem) (store.Location, error) {
	file, err := os.Open(item.LocalPath)
	if err != nil {
		return store.Location{}, err
	}
	defer file.Close()

	coordinates, found, err := geo.Read(file)
	if err != nil {
		return store.Location{}, err
	}
	return store.Location{MediaKey: item.MediaKey, Latitude: coordinates.Latitude,
		Longitude: coordinates.Longitude, Known: found}, nil
}
