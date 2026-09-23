// Package syncer runs one backup pass: list what Google holds, work out what is owed, and
// download it politely enough that a personal account never looks like a scraper.
package syncer

import (
	"context"
	"errors"
	"io"
	"log"
	"slices"
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
	// SkippedAlbums names the albums whose contents would not decode, and UnlistedAlbums the ones
	// Google has stopped listing at all. Both went unread and the run finished anyway, but they
	// ask different things of the user — a bug report and an unfollow — so they are counted apart.
	// Both are descriptions rather than ids because this is what the run page shows a person, and
	// an id on its own tells them nothing about what went unread.
	SkippedAlbums  []string
	UnlistedAlbums []string
	Outcome        store.Outcome
	Err            error
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
		log.Printf("syncer: discarded %d leftovers of interrupted downloads and links", discarded)
	}
}

// listAndDownload runs both halves of a backup at once, starting the workers on the backlog
// while the listing is still discovering the rest.
func (s *Syncer) listAndDownload(ctx context.Context, backlog []store.MediaItem, report Report) (Report, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var listed listingResult
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

	report.Listed = listed.listed
	report.SkippedAlbums = listed.skipped
	report.UnlistedAlbums = listed.unlisted
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
