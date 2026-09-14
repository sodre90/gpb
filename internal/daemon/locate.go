package daemon

import (
	"context"
	"log"
	"time"

	"gpb/internal/syncer"
)

// locateCheckInterval is how often the sweep asks whether any file is still waiting to be
// read for its place once it has caught up. New downloads are read as they land, so what is
// left for the sweep is a file that could not be opened at the time.
const locateCheckInterval = time.Hour

// locateProgressEvery is how many files the sweep reads between log lines. The first sweep
// over a library of a hundred thousand takes hours on a spinning disk, and a log that says
// nothing for that long looks like a sweep that stopped.
const locateProgressEvery = 5000

// locateFiles reads the place out of every backed-up file that has not been read yet, from
// the daemon's start and then whenever something is left. One file at a time, on one
// goroutine: the reading is disk-bound and shares the disk with the nightly run.
func (d *Daemon) locateFiles(ctx context.Context) {
	for {
		d.locateEverythingWaiting(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(locateCheckInterval):
		}
	}
}

func (d *Daemon) locateEverythingWaiting(ctx context.Context) {
	read, sinceLogged := 0, 0
	for ctx.Err() == nil {
		count, err := syncer.LocateBatch(ctx, d.store)
		if err != nil {
			log.Printf("locator: reading files for their places: %v", err)
			return
		}
		if count == 0 {
			break
		}
		read += count
		sinceLogged += count
		if sinceLogged >= locateProgressEvery {
			sinceLogged = 0
			d.logLocateProgress()
		}
	}
	if read > 0 {
		d.logLocateProgress()
	}
}

func (d *Daemon) logLocateProgress() {
	progress, err := d.store.LocationProgress()
	if err != nil {
		log.Printf("locator: counting what has been read: %v", err)
		return
	}
	log.Printf("locator: %d of %d backed-up files read for a place, %d had one",
		progress.Read, progress.OnDisk, progress.Located)
}
