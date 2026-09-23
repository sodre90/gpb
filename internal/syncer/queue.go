package syncer

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"time"

	"gpb/internal/store"
)

// reclaimVanishedFiles puts back on the work list anything the store calls done but the disk no
// longer has — DESIGN.md §9, "selected ∧ done but file missing → re-download". Without it the
// store's word is the only evidence a backup exists, and a file lost to a bad disk, a careless
// rsync or a half-restored snapshot is a gap nothing would ever notice, least of all the page
// that reports everything backed up.
func (s *Syncer) reclaimVanishedFiles() error {
	kept, err := s.store.DownloadedInSyncSet()
	if err != nil {
		return err
	}

	var lost int
	for _, item := range kept {
		if item.LocalPath == "" {
			continue
		}
		if _, err := os.Stat(item.LocalPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			// An unreadable path is not a missing file, and re-downloading on the strength of a
			// permissions error would fetch the whole library the first time a mount went odd.
			log.Printf("syncer: could not check %s: %v", item.MediaKey, err)
			continue
		}
		if err := s.store.SetItemState(item.MediaKey, store.StateDiscovered); err != nil {
			return err
		}
		lost++
	}

	if lost > 0 {
		log.Printf("syncer: %d backed-up files are gone from disk and will be fetched again", lost)
	}
	return nil
}

// download runs the work list across a small worker pool. The breaker shares a cancellable
// context with the workers, so tripping it stops the whole run rather than only the worker
// that noticed.
func (s *Syncer) download(ctx context.Context, backlog []store.MediaItem,
	listingDone <-chan struct{}, report Report) (Report, error) {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	breaker := NewBreaker(s.options.BreakerThreshold)
	work := make(chan store.MediaItem)

	var mu sync.Mutex
	var workers sync.WaitGroup

	for range max(1, s.options.Workers) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range work {
				size, err := s.downloadOne(ctx, item)

				mu.Lock()
				switch {
				case err == nil:
					report.Downloaded++
					report.Bytes += size
					s.live.downloaded.Add(1)
				case errors.Is(err, errNoFollowedAlbum):
					report.Skipped++
				default:
					report.Failed++
					s.live.failed.Add(1)
				}
				mu.Unlock()

				if err == nil {
					breaker.Succeed()
					continue
				}
				if breaker.Fail(err) {
					log.Printf("syncer: circuit breaker tripped after %d consecutive failures: %v",
						s.options.BreakerThreshold, err)
					cancel()
				}
			}
		}()
	}

	noRoom := s.feed(ctx, work, backlog, listingDone)
	close(work)
	workers.Wait()

	switch {
	case breaker.Tripped() != nil:
		return report, breaker.Tripped()
	case ctx.Err() != nil:
		return report, ctx.Err()
	case noRoom != nil:
		return report, noRoom
	case report.Failed > 0:
		report.Outcome = store.OutcomePartial
	}
	return report, nil
}

// feed hands the workers everything this run owes. The work list is not known when the run
// starts, because the listing is still discovering it, so the feeder goes back to the store
// whenever it has handed out everything it was given. It stops once a query taken after the
// listing ended turns up nothing the workers have not already been offered.
func (s *Syncer) feed(ctx context.Context, work chan<- store.MediaItem,
	backlog []store.MediaItem, listingDone <-chan struct{}) error {

	offered := map[string]bool{}
	batch, listingOver := backlog, false
	s.noteOwed(len(batch))

	for {
		fresh, err := s.offer(ctx, work, batch, offered)
		if err != nil {
			return err
		}
		if len(offered) >= s.options.MaxItemsPerRun || ctx.Err() != nil {
			return nil
		}
		if fresh == 0 {
			if listingOver {
				return nil
			}
			if !s.waitBeforeAskingAgain(ctx, listingDone) {
				return nil
			}
		}

		// Sampled before the query, so that a listing which ends during the query is treated as
		// still running and gets one more look. The other order would drop whatever it wrote last.
		listingOver = hasClosed(listingDone)
		if batch, err = s.store.Pending(s.options.MaxItemFailures, s.options.MaxItemsPerRun); err != nil {
			log.Printf("syncer: could not re-read the work list: %v", err)
			return nil
		}
		s.noteOwed(len(batch))
	}
}

// offer hands out the items of one batch that have not been handed out already, and reports how
// many those were. A batch with nothing fresh in it means the store holds nothing the workers
// have not already been given — items in flight come back from Pending until they finish.
func (s *Syncer) offer(ctx context.Context, work chan<- store.MediaItem,
	batch []store.MediaItem, offered map[string]bool) (int, error) {

	fresh := 0
	for _, item := range batch {
		if offered[item.MediaKey] {
			continue
		}
		if len(offered) >= s.options.MaxItemsPerRun {
			return fresh, nil
		}
		// The feeder is the one place that sees every item before it is fetched, and it is single
		// threaded, so the space check belongs here rather than in each worker.
		if noRoom := s.roomToContinue(); noRoom != nil {
			log.Printf("syncer: stopping this run: %v", noRoom)
			return fresh, noRoom
		}

		select {
		case work <- item:
			offered[item.MediaKey] = true
			fresh++
		case <-ctx.Done():
			return fresh, nil
		}
	}
	return fresh, nil
}

// waitBeforeAskingAgain paces the feeder when the workers have run dry but the listing has not
// finished. The listing ending cuts the wait short, so a run does not sit out an interval it is
// no longer waiting for anything. It reports whether the run is still worth continuing.
func (s *Syncer) waitBeforeAskingAgain(ctx context.Context, listingDone <-chan struct{}) bool {
	timer := time.NewTimer(s.options.AskAgainAfter)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-listingDone:
		return true
	case <-ctx.Done():
		return false
	}
}

func hasClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

var errNoFollowedAlbum = errors.New("the item is no longer in any followed album")

// downloadOne retries a single item with backoff. It records the outcome in the store before
// returning, so a crash between here and the end of the run does not lose the fact.
func (s *Syncer) downloadOne(ctx context.Context, item store.MediaItem) (int64, error) {
	albumID, err := s.store.AlbumForItem(item.MediaKey)
	if err != nil {
		return 0, err
	}
	if albumID == "" {
		return 0, errNoFollowedAlbum
	}

	if err := s.store.SetItemState(item.MediaKey, store.StateDownloading); err != nil {
		return 0, err
	}

	flight := s.inFlight.start(item, albumID)
	defer flight.done()

	var lastErr error
	for attempt := range max(1, s.options.MaxAttempts) {
		if attempt > 0 {
			if err := s.pause(ctx, s.options.Backoff.Delay(attempt-1, retryAfterOf(lastErr))); err != nil {
				return 0, s.recordFailure(ctx, item.MediaKey, err)
			}
		}

		result, err := s.fetch(ctx, item, permissionAlbum(albumID), flight)
		if err == nil {
			return result.Size, s.recordSuccess(item, result)
		}

		lastErr = err
		if !Retryable(err) {
			break
		}
		log.Printf("syncer: attempt %d for an item failed, will retry: %v", attempt+1, err)
	}

	return 0, s.recordFailure(ctx, item.MediaKey, lastErr)
}

// permissionAlbum is what the download RPC is told to check the item against. The library is
// this store's own row and not an album Google has ever heard of, so an item covered only by it
// is asked for with no album at all.
func permissionAlbum(albumID string) string {
	if albumID == store.LibraryID {
		return ""
	}
	return albumID
}

func (s *Syncer) recordSuccess(item store.MediaItem, result downloaded) error {
	landed := store.MediaItem{
		MediaKey:  item.MediaKey,
		Filename:  result.Filename,
		LocalPath: result.Path,
		SizeBytes: result.Size,
		SHA256:    result.SHA256,
		MimeType:  result.ContentType,
	}
	if err := s.store.MarkDownloaded(landed, time.Now()); err != nil {
		return err
	}
	// The place is read while the file is still in the page cache; the sweep would get to it
	// eventually, but a photo from today's trip should be findable tonight.
	return LocateOne(s.store, landed)
}

// recordFailure returns the original cause, not the bookkeeping error: the caller needs to
// know why the download failed, and a store problem on top of it is a separate log line.
//
// A run being torn down is not the item's failure and is not charged as one. fail_count is
// permanent — nothing in the daemon, the CLI or the schedule ever resets it — and Pending
// stops offering an item once it is high enough, so charging everything in flight at every
// shutdown would retire a photo from the backup for the crime of having been mid-download
// during a handful of restarts. The item is left in 'downloading', which is the state the
// engine already requeues from after a crash.
//
// The run's own context is the test rather than the shape of the error, because a download
// has a two-hour deadline of its own: that one expiring is a real failure of a real item.
func (s *Syncer) recordFailure(ctx context.Context, mediaKey string, cause error) error {
	if cause == nil {
		cause = errors.New("the download failed for an unrecorded reason")
	}
	if ctx.Err() != nil {
		return cause
	}
	if err := s.store.MarkFailed(mediaKey, cause); err != nil {
		log.Printf("syncer: could not record a failure: %v", err)
	}
	return cause
}

func (s *Syncer) pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
