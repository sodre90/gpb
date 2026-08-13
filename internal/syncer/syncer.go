// Package syncer runs one backup pass: list what Google holds, work out what is owed, and
// download it politely enough that a personal account never looks like a scraper.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"gpb/internal/gphotos"
	"gpb/internal/store"
)

// Source is the upstream half of a run. *gphotos.Client satisfies it; tests supply a fake so
// the engine's retry, breaker and write protocol can be exercised without a network.
type Source interface {
	AccountID() string
	Albums(ctx context.Context, pageToken string) (gphotos.AlbumPage, error)
	SharedAlbums(ctx context.Context, pageToken string) (gphotos.AlbumPage, error)
	AlbumItems(ctx context.Context, albumID, pageToken string) (gphotos.ItemPage, error)
	Timeline(ctx context.Context, pageToken string) (gphotos.ItemPage, error)
	DownloadOriginal(ctx context.Context, mediaKey, albumID string, offset int64, w io.Writer) (gphotos.Download, error)
}

// Options are the politeness dials. Every default is chosen to be slow: the failure mode of
// being too fast is losing the account, and the failure mode of being too slow is that a
// nightly backup takes longer overnight.
type Options struct {
	PoolDir           string
	TempDir           string
	Workers           int
	RequestsPerSecond float64
	Burst             int
	MaxItemFailures   int
	BreakerThreshold  int
	MaxItemsPerRun    int
	Backoff           Backoff
	MaxAttempts       int
	// MinFreeBytes is the headroom the pool's filesystem must keep. A run stops when it would
	// eat into it, rather than discovering the end of the disk one failed write at a time.
	MinFreeBytes int64
	// AskAgainAfter is how long the feeder waits before asking the store for more work when the
	// workers have run dry and the listing is still going. It only matters on a first run, when
	// there is no backlog to be getting on with; after that the store always has more to offer
	// than the workers can take.
	AskAgainAfter time.Duration
}

func DefaultOptions(poolDir, tempDir string) Options {
	return Options{
		PoolDir:           poolDir,
		TempDir:           tempDir,
		Workers:           3,
		RequestsPerSecond: 2,
		Burst:             2,
		MaxItemFailures:   5,
		BreakerThreshold:  8,
		MaxItemsPerRun:    100000,
		Backoff:           DefaultBackoff(),
		MaxAttempts:       4,
		MinFreeBytes:      DiskFloor,
		AskAgainAfter:     15 * time.Second,
	}
}

type Syncer struct {
	source   Source
	store    *store.Store
	poolDir  string
	tempDir  string
	options  Options
	limiter  *rate.Limiter
	live     liveCounts
	inFlight *inFlightBoard
}

// Progress is a run's tally while it is still running. The durable counts are written once, at
// the end, by FinishRun — so without this the one number a person watching a backup wants does
// not exist until the moment it stops mattering.
type Progress struct {
	// RunID names the run these counts belong to, and is zero for a job that opened no run row —
	// an album listing, a refresh — or for a sync that has not opened one yet. It is what lets a
	// page tell the run that is working from the one the daemon was killed under, which otherwise
	// look identical: both are rows with no finish time.
	RunID      int64
	Listed     int
	Downloaded int
	Failed     int
	// Owed is how much this run has to download: what the store still held as pending when the
	// feeder last asked, plus what this run has already brought down. It grows while the listing
	// is still discovering work, which is why it is read from the store rather than fixed at the
	// start.
	Owed  int
	Items []InFlight
}

// Equal is how the web server tells one moment of a run from the next. Progress was comparable
// with == until it started carrying the list of what is in flight.
func (p Progress) Equal(other Progress) bool {
	return p.RunID == other.RunID &&
		p.Listed == other.Listed &&
		p.Downloaded == other.Downloaded &&
		p.Failed == other.Failed &&
		p.Owed == other.Owed &&
		slices.Equal(p.Items, other.Items)
}

type liveCounts struct {
	runID      atomic.Int64
	listed     atomic.Int64
	downloaded atomic.Int64
	failed     atomic.Int64
	owed       atomic.Int64
}

// Progress is read from another goroutine — the web request serving the page that is watching
// this run — while the workers are still writing to it.
func (s *Syncer) Progress() Progress {
	return Progress{
		RunID:      s.live.runID.Load(),
		Listed:     int(s.live.listed.Load()),
		Downloaded: int(s.live.downloaded.Load()),
		Failed:     int(s.live.failed.Load()),
		Owed:       int(s.live.owed.Load()),
		Items:      s.inFlight.snapshot(),
	}
}

// noteOwed records the size of the work list from the one place that knows it: the feeder, which
// re-reads what is still pending whenever it has handed out everything it was given.
func (s *Syncer) noteOwed(pending int) {
	s.live.owed.Store(s.live.downloaded.Load() + int64(pending))
}

// Report is what a run tells the outside world. It is returned even when the run fails, so a
// caller can record the work that did land before the failure.
type Report struct {
	RunID      int64
	Listed     int
	Downloaded int
	Failed     int
	Skipped    int
	Bytes      int64
	Outcome    store.Outcome
	Err        error
}

func New(source Source, db *store.Store, options Options) *Syncer {
	return &Syncer{
		source:   source,
		store:    db,
		poolDir:  options.PoolDir,
		tempDir:  options.TempDir,
		options:  options,
		limiter:  rate.NewLimiter(rate.Limit(options.RequestsPerSecond), options.Burst),
		inFlight: newInFlightBoard(),
	}
}

// Limiter is the shared token bucket. The gphotos client waits on it too, so listing and
// downloading draw from one budget instead of quietly doubling the request rate.
func (s *Syncer) Limiter() *rate.Limiter {
	return s.limiter
}

// Run performs one backup pass and always writes its report to the store, including on failure:
// a run that died is more interesting than one that never happened.
//
// The listing and the downloading happen at the same time. They used to be sequential, which
// meant nothing was fetched until every album and the whole library timeline had been walked —
// on a large account that is the better part of an hour in which a run appears to do nothing.
// Downloading only needs the work list to be non-empty, and on any run but the first it is
// non-empty before the run starts. Both halves wait on the one rate limiter, so doing them
// together does not ask Google for anything faster than doing them in turn did.
func (s *Syncer) Run(ctx context.Context) (Report, error) {
	startedAt := time.Now()
	runID, err := s.store.StartRun(startedAt)
	if err != nil {
		return Report{}, err
	}
	s.live.runID.Store(runID)
	report := Report{RunID: runID, Outcome: store.OutcomeOK}

	if err := s.reclaimVanishedFiles(); err != nil {
		return s.finish(report, err)
	}

	backlog, err := s.store.Pending(s.options.MaxItemFailures, s.options.MaxItemsPerRun)
	if err != nil {
		return s.finish(report, err)
	}
	log.Printf("syncer: %d items owed before this run lists anything", len(backlog))

	return s.finish(s.listAndDownload(ctx, backlog, report))
}

// tidyStagingDirectory drops partial downloads for items no longer owed. It belongs at the end
// of a run: it judges a part file by whether the work list still wants it, which is only a fair
// question once the listing has finished, and it deletes files, which is only safe once the
// workers have stopped. Running it first — as it did when a run listed everything before
// fetching anything — would now throw away the resume point of an item not yet listed.
func (s *Syncer) tidyStagingDirectory() {
	pending, err := s.store.Pending(s.options.MaxItemFailures, s.options.MaxItemsPerRun)
	if err != nil {
		log.Printf("syncer: could not read the work list to tidy the staging directory: %v", err)
		return
	}

	discarded, err := discardStaleParts(s.tempDir, pendingKeys(pending))
	if err != nil {
		log.Printf("syncer: could not tidy the staging directory: %v", err)
		return
	}
	if discarded > 0 {
		log.Printf("syncer: discarded %d stale partial downloads", discarded)
	}
}

// listAndDownload runs both halves of a backup at once, starting the workers on the backlog
// while the listing is still discovering the rest.
func (s *Syncer) listAndDownload(ctx context.Context, backlog []store.MediaItem, report Report) (Report, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var listed int
	var listErr error
	listingDone := make(chan struct{})
	go func() {
		defer close(listingDone)
		listed, listErr = s.list(ctx)
	}()

	report, downloadErr := s.download(ctx, backlog, listingDone, report)

	// A download that stopped early — a full disk, a tripped breaker — leaves the listing walking
	// an account whose photos nothing is going to fetch, so it is cancelled rather than waited out.
	cancel()
	<-listingDone
	s.tidyStagingDirectory()

	report.Listed = listed
	return report, worseOf(downloadErr, listErr)
}

// worseOf picks which half's failure the run is reported as. Cancellation is the least
// interesting answer either half can give, because the other half is usually what caused it.
func worseOf(downloadErr, listErr error) error {
	switch {
	case downloadErr != nil && !errors.Is(downloadErr, context.Canceled):
		return downloadErr
	case listErr != nil && !errors.Is(listErr, context.Canceled):
		return listErr
	case downloadErr != nil:
		return downloadErr
	default:
		return listErr
	}
}

func pendingKeys(items []store.MediaItem) map[string]bool {
	keys := make(map[string]bool, len(items))
	for _, item := range items {
		keys[safeName(item.MediaKey)] = true
	}
	return keys
}

// RefreshAlbums records every album Google reports, downloading nothing. A run needs this to
// know what it follows, and the album picker needs it to offer a current picture of what
// there is to follow, so it is worth doing on its own.
// It reads two listings because neither is the library on its own. The album listing carries
// albums this account owns plus nameless bundles of shared photos; the shared listing repeats
// those albums and adds the ones other people shared, which the first listing omits entirely.
func (s *Syncer) RefreshAlbums(ctx context.Context) error {
	seenAt := time.Now()

	owned, err := s.recordListing(ctx, s.source.Albums, seenAt, nil)
	if err != nil {
		return fmt.Errorf("listing albums: %w", err)
	}
	// The shared listing's own entries do not say which of the two they are — an album this
	// account owns looks exactly like one shared with it — so the album listing having already
	// claimed an id is what separates them.
	if _, err := s.recordListing(ctx, s.source.SharedAlbums, seenAt, owned); err != nil {
		return fmt.Errorf("listing shared albums: %w", err)
	}
	return nil
}

type listing func(ctx context.Context, pageToken string) (gphotos.AlbumPage, error)

// recordListing walks one paginated listing, skipping ids already claimed, and reports every
// id it saw so a later listing can tell new albums from repeats.
func (s *Syncer) recordListing(ctx context.Context, fetch listing,
	seenAt time.Time, claimed map[string]bool) (map[string]bool, error) {

	seen := map[string]bool{}
	for token := ""; ; {
		page, err := fetch(ctx, token)
		if err != nil {
			return nil, err
		}
		for _, album := range page.Albums {
			if claimed[album.ID] {
				continue
			}
			if album.Kind == "" {
				album.Kind = gphotos.AlbumShared
			}
			if err := s.store.UpsertAlbum(toStoreAlbum(album, s.source.AccountID()), seenAt); err != nil {
				return nil, err
			}
			seen[album.ID] = true
		}
		if token = page.NextToken; token == "" {
			return seen, nil
		}
	}
}

// list refreshes every album, then walks the followed ones and, if it is followed, the library.
//
// Every run walks everything again rather than resuming where the last one stopped, and that is
// deliberate: seeing all of an album is what lets listAlbum infer that the items it did not see
// have gone from Google. A resumable walk would be faster and would never notice a deletion.
//
// Each phase is timed because the run log used to print one total at the end, which said nothing
// about where a long run spent its time.
func (s *Syncer) list(ctx context.Context) (int, error) {
	startedAt := time.Now()
	if err := s.RefreshAlbums(ctx); err != nil {
		return 0, err
	}

	followed, err := s.store.FollowedAlbums()
	if err != nil {
		return 0, err
	}
	log.Printf("syncer: %d albums to walk, listed in %s", len(followed), since(startedAt))

	albumsStartedAt := time.Now()
	listed := 0
	for _, album := range followed {
		walkStartedAt := time.Now()
		count, err := s.listAlbum(ctx, album.ID)
		if err != nil {
			return listed, err
		}
		log.Printf("syncer: an album walk recorded %d items in %s", count, since(walkStartedAt))
		listed += count
	}
	albumTime := since(albumsStartedAt)

	library, err := s.store.Library()
	if err != nil {
		return listed, err
	}
	if library.SyncMode == store.SyncNone {
		log.Printf("syncer: listing finished in %s — %d albums in %s, the library is not followed",
			since(startedAt), len(followed), albumTime)
		return listed, nil
	}

	libraryStartedAt := time.Now()
	count, err := s.listLibrary(ctx, library.Since)
	log.Printf("syncer: listing finished in %s — %d albums in %s, the library walk in %s",
		since(startedAt), len(followed), albumTime, since(libraryStartedAt))
	return listed + count, err
}

// since rounds to the second because these are phases measured in minutes and nobody reading a
// run log needs the nanoseconds.
func since(startedAt time.Time) time.Duration {
	return time.Since(startedAt).Round(time.Second)
}

// listLibrary walks the timeline — everything the account holds, newest capture first — down to
// the date the user set. It is the only way to reach a photo that is in no album, which on a
// real library is most of them.
//
// Unlike an album walk, this one usually stops early by design, so what it reconciles has to be
// limited to the part of the timeline it actually reached — see reconcilableFrom.
func (s *Syncer) listLibrary(ctx context.Context, since time.Time) (int, error) {
	listedAt := time.Now()
	listed := 0
	walkedItAll := false

	for token := ""; ; {
		page, err := s.source.Timeline(ctx, token)
		if err != nil {
			return listed, fmt.Errorf("listing the library: %w", err)
		}

		for _, item := range page.Items {
			if olderThan(item, since) {
				continue
			}
			if err := s.store.UpsertItem(toStoreItem(item), listedAt); err != nil {
				return listed, err
			}
			if err := s.store.LinkItemToAlbum(store.LibraryID, item.MediaKey, listedAt); err != nil {
				return listed, err
			}
			listed++
			s.live.listed.Add(1)
		}

		if token = page.NextToken; token == "" {
			walkedItAll = true
			break
		}
		if walkedPastTheBound(page.Items, since) {
			break
		}
	}

	log.Printf("syncer: the library walk recorded %d items", listed)
	if err := s.reconcileLibrary(listed, reconcilableFrom(since, walkedItAll), listedAt); err != nil {
		return listed, err
	}
	return listed, s.store.MarkAlbumSynced(store.LibraryID, listedAt)
}

// reconcileLibrary writes off the photos the timeline no longer lists. It is the only thing that
// ever notices a deletion for a photo in no album, and most of a library is in no album.
//
// A walk that listed nothing reconciles nothing. An account under backup is never empty, so a
// timeline that answers with no items is a failure wearing a success's clothes — and taken at its
// word it would write off every photo the user has.
func (s *Syncer) reconcileLibrary(listed int, capturedFrom, listedAt time.Time) error {
	if listed == 0 {
		log.Print("syncer: the library walk listed nothing, so nothing is written off")
		return nil
	}

	departed, err := s.store.ReconcileLibrary(capturedFrom, listedAt)
	if err != nil {
		return err
	}
	if departed.LeftTheAlbum > 0 {
		log.Printf("syncer: %d items left the library, %d of them gone from Google",
			departed.LeftTheAlbum, departed.GoneFromGoogle)
	}
	return nil
}

// boundaryWobble is how far out of order a capture time can sit around the date bound. The times
// carry the camera's own timezone, so the same instant is worth a day either way in the order
// Google serves — see walkedPastTheBound, which reads the same wobble from the other side.
const boundaryWobble = 48 * time.Hour

// reconcilableFrom is the oldest capture date a walk can be believed about. One that reached the
// end of the timeline saw everything above the bound; one that stopped at the bound saw
// everything except what the wobble there could have pushed onto a page it never asked for, so it
// keeps that far clear of it. Below the bound nothing is reconcilable at all: the walk skipped
// those items rather than failing to find them.
func reconcilableFrom(since time.Time, walkedItAll bool) time.Time {
	if walkedItAll {
		return since
	}
	return since.Add(boundaryWobble)
}

// olderThan applies the library's date bound. An item whose capture date Google did not report
// is never older than anything: the bound is there to stop a walk into the archive, not to
// quietly drop the photos with the worst metadata.
func olderThan(item gphotos.MediaItem, since time.Time) bool {
	captured := item.LocalCaptureTime()
	return !since.IsZero() && !captured.IsZero() && captured.Before(since)
}

// walkedPastTheBound ends the walk once a whole page has fallen behind the date rather than at
// the first item that has. The timeline is ordered by capture time, but those times carry the
// camera's own timezone, so around the boundary the order can wobble by the better part of a day.
func walkedPastTheBound(page []gphotos.MediaItem, since time.Time) bool {
	return len(page) > 0 && olderThan(page[len(page)-1], since)
}

// ListAlbum records one album's contents without downloading anything. The item grid needs
// it: an album nobody follows has never been walked, so there is nothing to pick from until
// someone asks Google for its contents.
func (s *Syncer) ListAlbum(ctx context.Context, albumID string) (int, error) {
	return s.listAlbum(ctx, albumID)
}

// listAlbum pages one album to the end and then reconciles: anything previously linked but
// not seen in this pass has gone from the album upstream. The full walk is what makes that
// inference safe, so a partial listing must never reach the reconciliation step.
func (s *Syncer) listAlbum(ctx context.Context, albumID string) (int, error) {
	listedAt := time.Now()
	listed := 0

	alreadyIn, err := s.membersToCompareAgainst(albumID)
	if err != nil {
		return listed, err
	}
	var arrived []string

	for token := ""; ; {
		page, err := s.source.AlbumItems(ctx, albumID, token)
		if err != nil {
			return listed, fmt.Errorf("listing album items: %w", err)
		}

		for _, item := range page.Items {
			if err := s.store.UpsertItem(toStoreItem(item), listedAt); err != nil {
				return listed, err
			}
			if err := s.store.LinkItemToAlbum(albumID, item.MediaKey, listedAt); err != nil {
				return listed, err
			}
			if alreadyIn != nil && !alreadyIn[item.MediaKey] {
				arrived = append(arrived, item.MediaKey)
			}
			listed++
			s.live.listed.Add(1)
		}

		if token = page.NextToken; token == "" {
			break
		}
	}

	if err := s.flagArrivals(albumID, arrived); err != nil {
		return listed, err
	}

	departed, err := s.store.ReconcileAlbum(albumID, listedAt)
	if err != nil {
		return listed, err
	}
	if departed.LeftTheAlbum > 0 {
		log.Printf("syncer: %d items left an album, %d of them gone from Google",
			departed.LeftTheAlbum, departed.GoneFromGoogle)
	}

	return listed, s.store.MarkAlbumSynced(albumID, listedAt)
}

// membersToCompareAgainst returns what the album held before this listing, or nil when nothing
// arriving in it could need a decision. Two albums answer nil: one synced 'all' or 'none', where
// a new item is downloaded or ignored without anyone being asked, and one never walked before,
// where every item would look new and the user would be handed a review queue the size of the
// album for merely having pressed refresh. The first walk is the baseline, not a change to it.
func (s *Syncer) membersToCompareAgainst(albumID string) (map[string]bool, error) {
	album, err := s.store.Album(albumID)
	if err != nil {
		return nil, err
	}
	if album.SyncMode != store.SyncPicked || album.LastSyncedAt.IsZero() {
		return nil, nil
	}
	return s.store.MembersOf(albumID)
}

// flagArrivals holds back items that turned up in a 'picked' album since it was last walked.
// DESIGN.md §4: they are recorded but not downloaded, because the user picks from a 'picked'
// album and nobody has picked these yet.
func (s *Syncer) flagArrivals(albumID string, arrived []string) error {
	if len(arrived) == 0 {
		return nil
	}
	if err := s.store.FlagForReview(arrived); err != nil {
		return err
	}
	log.Printf("syncer: %d new items in a picked album are waiting for a decision", len(arrived))
	return nil
}

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
	return s.store.MarkDownloaded(store.MediaItem{
		MediaKey:  item.MediaKey,
		Filename:  result.Filename,
		LocalPath: result.Path,
		SizeBytes: result.Size,
		SHA256:    result.SHA256,
		MimeType:  result.ContentType,
	}, time.Now())
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

func (s *Syncer) finish(report Report, err error) (Report, error) {
	report.Err = err
	report.Outcome = outcomeOf(report, err)

	if storeErr := s.store.FinishRun(store.SyncRun{
		ID:         report.RunID,
		Outcome:    report.Outcome,
		Listed:     report.Listed,
		Downloaded: report.Downloaded,
		Failed:     report.Failed,
		Bytes:      report.Bytes,
		Error:      errorText(err),
	}, time.Now()); storeErr != nil {
		log.Printf("syncer: could not record the run: %v", storeErr)
	}
	return report, err
}

// outcomeOf separates the failures that need a human from the ones that need a retry.
// OutcomeOf classifies a failure that happened before a run could report for itself — while
// taking the run lock, warming the profile or connecting to Google. Those are recorded as runs
// of their own, and they deserve the same reading of the same errors as a run that got further:
// above all, a cancelled setup is a shutdown rather than a fault.
func OutcomeOf(err error) store.Outcome {
	return outcomeOf(Report{}, err)
}

func outcomeOf(report Report, err error) store.Outcome {
	switch {
	case errors.Is(err, gphotos.ErrSessionRejected):
		return store.OutcomeAuthRequired
	case errors.Is(err, gphotos.ErrProtocolDrift):
		return store.OutcomeDrift
	// A cancelled context means this process was asked to stop, which is a shutdown and not a
	// fault. Recording it as an error would leave the daemon believing the day's backup had been
	// attempted and answered, when in truth it was cut off partway.
	case errors.Is(err, context.Canceled):
		return store.OutcomeInterrupted
	case err != nil:
		return store.OutcomeError
	case report.Failed > 0:
		return store.OutcomePartial
	default:
		return store.OutcomeOK
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// toStoreAlbum resolves the owner against the signed-in account here, because this is the only
// layer that holds both. An empty accountID — a page shell that stopped carrying it — leaves
// every album owned by someone named rather than silently claiming they are all the user's.
func toStoreAlbum(album gphotos.Album, accountID string) store.Album {
	return store.Album{
		ID:             album.ID,
		Title:          album.Title,
		ItemCount:      album.ItemCount,
		CreatedAt:      album.CreatedAt,
		Kind:           store.AlbumKind(album.Kind),
		CoverURL:       album.CoverURL,
		OwnerName:      album.Owner.Name,
		OwnerIsAccount: accountID != "" && album.Owner.ID == accountID,
	}
}

// toStoreItem leaves the filename empty on purpose. Google's listing does not carry one — the
// name arrives with the download, in its Content-Disposition — and standing the media key in its
// place made every page that shows a name show an identifier instead.
func toStoreItem(item gphotos.MediaItem) store.MediaItem {
	return store.MediaItem{
		MediaKey:     item.MediaKey,
		CapturedAt:   item.LocalCaptureTime(),
		ThumbnailURL: item.ThumbnailURL,
		IsVideo:      item.IsVideo,
	}
}
