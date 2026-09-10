package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpb/internal/gphotos"
	"gpb/internal/store"
)

// fakeSource stands in for Google. It serves fixed bytes per media key and can be told to
// fail a given number of times first, which is how the retry and breaker paths get exercised
// without a network or a clock.
type fakeSource struct {
	accountID    string
	albums       []gphotos.Album
	sharedAlbums []gphotos.Album
	items        map[string][]gphotos.MediaItem
	bodies       map[string][]byte
	// timeline is the library listing, held as pages so a test can prove the walk stops at the
	// date bound instead of paging on into the archive.
	timeline [][]gphotos.MediaItem

	// beforeAlbumItems and onDownload let a test hold one half of a run still while the other
	// half proves it is not waiting for it. Both are set before Run and never after.
	beforeAlbumItems func()
	onDownload       func()
	albumItemsErr    error
	// albumItemsErrs fails named albums while the rest still list, which is the shape of every
	// question about skipping: one album Google answers strangely among many it answers well.
	albumItemsErrs map[string]error

	mu             sync.Mutex
	failures       map[string]int
	ignoreRange    map[string]bool
	downloadCalls  int
	timelineCalls  int
	albumsListed   []string
	albumsAskedFor []string
	rangeRequested []int64
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		items:          map[string][]gphotos.MediaItem{},
		bodies:         map[string][]byte{},
		failures:       map[string]int{},
		ignoreRange:    map[string]bool{},
		albumItemsErrs: map[string]error{},
	}
}

func (f *fakeSource) AccountID() string { return f.accountID }

func (f *fakeSource) Albums(context.Context, string) (gphotos.AlbumPage, error) {
	return gphotos.AlbumPage{Albums: f.albums}, nil
}

func (f *fakeSource) SharedAlbums(context.Context, string) (gphotos.AlbumPage, error) {
	return gphotos.AlbumPage{Albums: f.sharedAlbums}, nil
}

func (f *fakeSource) AlbumItems(_ context.Context, albumID, _ string) (gphotos.ItemPage, error) {
	if f.beforeAlbumItems != nil {
		f.beforeAlbumItems()
	}
	f.mu.Lock()
	f.albumsListed = append(f.albumsListed, albumID)
	f.mu.Unlock()

	if f.albumItemsErr != nil {
		return gphotos.ItemPage{}, f.albumItemsErr
	}
	if err := f.albumItemsErrs[albumID]; err != nil {
		return gphotos.ItemPage{}, err
	}
	return gphotos.ItemPage{AlbumID: albumID, Items: f.items[albumID]}, nil
}

// Timeline serves the pages in order, naming the next one by its index so the walk has a real
// cursor to follow rather than a token the fake ignores.
func (f *fakeSource) Timeline(_ context.Context, pageToken string) (gphotos.ItemPage, error) {
	index := 0
	if pageToken != "" {
		if _, err := fmt.Sscanf(pageToken, "page-%d", &index); err != nil {
			return gphotos.ItemPage{}, err
		}
	}
	if index >= len(f.timeline) {
		return gphotos.ItemPage{}, nil
	}

	f.mu.Lock()
	f.timelineCalls++
	f.mu.Unlock()

	page := gphotos.ItemPage{Items: f.timeline[index]}
	if index+1 < len(f.timeline) {
		page.NextToken = fmt.Sprintf("page-%d", index+1)
	}
	return page, nil
}

func (f *fakeSource) DownloadOriginal(ctx context.Context, mediaKey, albumID string, offset int64, w io.Writer) (gphotos.Download, error) {
	f.mu.Lock()
	f.downloadCalls++
	f.albumsAskedFor = append(f.albumsAskedFor, albumID)
	f.rangeRequested = append(f.rangeRequested, offset)
	remaining := f.failures[mediaKey]
	if remaining > 0 {
		f.failures[mediaKey] = remaining - 1
		f.mu.Unlock()
		return gphotos.Download{}, &gphotos.HTTPError{StatusCode: 503}
	}
	ignore := f.ignoreRange[mediaKey]
	f.mu.Unlock()

	body, ok := f.bodies[mediaKey]
	if !ok {
		return gphotos.Download{}, errors.New("no such item")
	}

	if offset > 0 && ignore {
		return gphotos.Download{}, gphotos.ErrRangeIgnored
	}
	if offset > int64(len(body)) {
		return gphotos.Download{}, gphotos.ErrRangeIgnored
	}

	// The real client tells the writer what the response says about the file before any of the
	// bytes flow. Without it here, the wiring the card's names and percentages travel down would
	// go unexercised.
	if announcer, ok := w.(gphotos.Announcer); ok {
		announcer.Announce(mediaKey+".jpg", int64(len(body)))
	}
	if f.onDownload != nil {
		f.onDownload()
	}

	// The real client abandons a transfer the moment the run is torn down. Ignoring the
	// context here left the shutdown path with no way to be exercised at all.
	if err := ctx.Err(); err != nil {
		return gphotos.Download{}, err
	}

	written, err := w.Write(body[offset:])
	if err != nil {
		return gphotos.Download{}, err
	}
	return gphotos.Download{
		Filename:    mediaKey + ".jpg",
		ContentType: "image/jpeg",
		Size:        offset + int64(written),
	}, nil
}

func (f *fakeSource) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downloadCalls
}

func (f *fakeSource) offsets() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.rangeRequested...)
}

func (f *fakeSource) albumsRequested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.albumsAskedFor...)
}

// albumsWalked names the albums a run asked for the contents of, in order. It answers the one
// question a test cannot ask any other way: whether the walk carried on past a bad album or
// stopped dead at it.
func (f *fakeSource) albumsWalked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.albumsListed...)
}

func (f *fakeSource) timelinePagesServed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.timelineCalls
}

type harness struct {
	syncer *Syncer
	source *fakeSource
	store  *store.Store
	pool   string
	temp   string
}

func newHarness(t *testing.T, mode store.SyncMode, bodies map[string]string) *harness {
	t.Helper()

	root := t.TempDir()
	pool := filepath.Join(root, "pool")
	temp := filepath.Join(root, ".tmp")

	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	source := newFakeSource()
	source.albums = []gphotos.Album{{ID: "album-1", Title: "Holiday", ItemCount: len(bodies)}}

	for key, body := range bodies {
		source.items["album-1"] = append(source.items["album-1"], gphotos.MediaItem{
			MediaKey:   key,
			CapturedAt: time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
			Width:      100, Height: 100,
		})
		source.bodies[key] = []byte(body)
	}

	options := DefaultOptions(pool, temp)
	// These downloads are a few bytes each, so the production free-space floor has nothing to
	// protect here — it would only make the suite fail on whichever machine happens to be full.
	// TestARunStopsBeforeEatingIntoTheFreeSpaceFloor sets its own.
	options.MinFreeBytes = 0
	options.RequestsPerSecond = 1000
	options.Burst = 1000
	options.Backoff = Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}

	syncer := New(source, db, options)

	if err := db.UpsertAlbum(store.Album{ID: "album-1", Title: "Holiday"}, time.Now()); err != nil {
		t.Fatalf("seeding the album: %v", err)
	}
	if err := db.SetAlbumSyncMode("album-1", mode); err != nil {
		t.Fatalf("setting the sync mode: %v", err)
	}

	return &harness{syncer: syncer, source: source, store: db, pool: pool, temp: temp}
}

func (h *harness) poolFiles(t *testing.T) []string {
	t.Helper()

	var found []string
	err := filepath.WalkDir(h.pool, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		relative, _ := filepath.Rel(h.pool, path)
		found = append(found, relative)
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("walking the pool: %v", err)
	}
	return found
}

func TestRunDownloadsAFollowedAlbumIntoThePool(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "photo a bytes", "key-b": "photo b bytes"})

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if report.Listed != 2 || report.Downloaded != 2 || report.Failed != 0 {
		t.Fatalf("the report is %+v", report)
	}

	files := h.poolFiles(t)
	if len(files) != 2 {
		t.Fatalf("the pool holds %v, want 2 files", files)
	}
	for _, file := range files {
		if !strings.HasPrefix(file, filepath.Join("2026", "2026-06")) {
			t.Errorf("%s is not filed under its capture month", file)
		}
	}

	item, err := h.store.Item("key-a")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if item.State != store.StateDone {
		t.Errorf("a downloaded item is in state %q", item.State)
	}

	want := sha256.Sum256([]byte("photo a bytes"))
	if item.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("the recorded hash does not match the bytes served")
	}
	if item.SizeBytes != int64(len("photo a bytes")) {
		t.Errorf("the recorded size is %d", item.SizeBytes)
	}
}

// The commit is a rename, so a crash can leave a .part but must never leave a half-written
// file in the pool. This checks the successful path cleans up after itself too.
func TestNoPartialFilesSurviveASuccessfulRun(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "bytes"})

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	remaining, err := os.ReadDir(h.temp)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading the staging directory: %v", err)
	}
	for _, entry := range remaining {
		if filepath.Ext(entry.Name()) == ".part" {
			t.Errorf("a staging file survived a successful run: %s", entry.Name())
		}
	}
}

// An interrupted download must resume rather than refetch, and the hash it records has to
// cover the whole file — including the bytes written by the previous process.
func TestAnInterruptedDownloadResumesAndHashesTheWholeFile(t *testing.T) {
	const body = "the complete original bytes"
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": body})

	if err := os.MkdirAll(h.temp, 0o755); err != nil {
		t.Fatalf("preparing the staging directory: %v", err)
	}
	prefix := body[:10]
	if err := os.WriteFile(partPath(h.temp, "key-a"), []byte(prefix), 0o644); err != nil {
		t.Fatalf("seeding a partial download: %v", err)
	}

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	if offsets := h.source.offsets(); len(offsets) != 1 || offsets[0] != int64(len(prefix)) {
		t.Fatalf("the download requested offsets %v, want a single resume at %d", offsets, len(prefix))
	}

	item, err := h.store.Item("key-a")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	want := sha256.Sum256([]byte(body))
	if item.SHA256 != hex.EncodeToString(want[:]) {
		t.Error("the resumed file's hash does not cover the whole file")
	}

	stored, err := os.ReadFile(item.LocalPath)
	if err != nil {
		t.Fatalf("reading the stored file: %v", err)
	}
	if string(stored) != body {
		t.Errorf("the stored file is %q", stored)
	}
}

// If the server ignores the Range header it sends the whole file again. Appending that to the
// partial would silently produce a corrupt double-length file.
func TestAServerThatIgnoresRangeCausesACleanRestart(t *testing.T) {
	const body = "the complete original bytes"
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": body})
	h.source.ignoreRange["key-a"] = true

	if err := os.MkdirAll(h.temp, 0o755); err != nil {
		t.Fatalf("preparing the staging directory: %v", err)
	}
	if err := os.WriteFile(partPath(h.temp, "key-a"), []byte(body[:10]), 0o644); err != nil {
		t.Fatalf("seeding a partial download: %v", err)
	}

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	item, err := h.store.Item("key-a")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	stored, err := os.ReadFile(item.LocalPath)
	if err != nil {
		t.Fatalf("reading the stored file: %v", err)
	}
	if string(stored) != body {
		t.Fatalf("the restarted file is %d bytes, want %d", len(stored), len(body))
	}
}

func TestATransientFailureIsRetried(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "bytes"})
	h.source.failures["key-a"] = 2

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if report.Downloaded != 1 {
		t.Fatalf("the report is %+v", report)
	}
	if h.source.calls() != 3 {
		t.Errorf("the item was attempted %d times, want 3", h.source.calls())
	}
}

// A failing item must not be retried forever, and the failure has to be recorded where the
// review queue can find it.
func TestAPermanentlyFailingItemIsRecordedAndGivenUpOn(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "bytes"})
	h.source.failures["key-a"] = 99

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if report.Failed != 1 || report.Outcome != store.OutcomePartial {
		t.Fatalf("the report is %+v", report)
	}
	if h.source.calls() != DefaultOptions("", "").MaxAttempts {
		t.Errorf("the item was attempted %d times", h.source.calls())
	}

	item, err := h.store.Item("key-a")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if item.State != store.StateFailed || item.FailCount != 1 || item.LastError == "" {
		t.Errorf("the failure was recorded as %+v", item)
	}
	if files := h.poolFiles(t); len(files) != 0 {
		t.Errorf("a failed download left %v in the pool", files)
	}
}

// fail_count is permanent — nothing resets it — and Pending stops offering an item once it is
// high enough. Charging an item for a shutdown it happened to be in flight for would mean a
// photo quietly leaving the backup for good after a handful of ordinary restarts, which would
// make SIGKILL gentler on the database than SIGTERM.
func TestAShutdownDoesNotChargeInFlightItemsAFailure(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "bytes"})
	ctx, cancel := context.WithCancel(t.Context())
	h.source.onDownload = cancel

	if _, err := h.syncer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("running the sync: %v", err)
	}

	item, err := h.store.Item("key-a")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if item.FailCount != 0 || item.State == store.StateFailed {
		t.Errorf("a shutdown charged an in-flight item: %d failures, state %q",
			item.FailCount, item.State)
	}

	owed, err := h.store.Pending(DefaultOptions("", "").MaxItemFailures, 10)
	if err != nil {
		t.Fatalf("reading the work list: %v", err)
	}
	if len(owed) != 1 {
		t.Errorf("the next run is owed %d items, want the interrupted one offered again", len(owed))
	}
}

// Grinding through a whole library against a broken server is how an account gets noticed.
func TestTheBreakerAbortsARunOfConsecutiveFailures(t *testing.T) {
	bodies := map[string]string{}
	for index := range 20 {
		bodies[fmt.Sprintf("key-%02d", index)] = "bytes"
	}
	h := newHarness(t, store.SyncAll, bodies)
	for key := range bodies {
		h.source.failures[key] = 99
	}

	report, err := h.syncer.Run(t.Context())
	if err == nil {
		t.Fatal("a run of nothing but failures reported success")
	}
	if report.Outcome != store.OutcomeError {
		t.Errorf("the run ended as %q", report.Outcome)
	}
	if report.Failed >= len(bodies) {
		t.Errorf("the breaker let all %d items fail before stopping", report.Failed)
	}
}

func TestUnfollowedAlbumsAreListedButNotDownloaded(t *testing.T) {
	h := newHarness(t, store.SyncNone, map[string]string{"key-a": "bytes"})

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if report.Listed != 0 || report.Downloaded != 0 {
		t.Fatalf("an unfollowed album produced %+v", report)
	}

	if _, err := h.store.Album("album-1"); err != nil {
		t.Errorf("the album was not recorded for the UI to offer: %v", err)
	}
}

func TestVanishedItemsAreMarkedByARun(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "bytes", "key-b": "bytes"})

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	h.source.items["album-1"] = h.source.items["album-1"][:1]
	gone := h.source.items["album-1"][0].MediaKey

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("second run: %v", err)
	}

	for _, key := range []string{"key-a", "key-b"} {
		item, err := h.store.Item(key)
		if err != nil {
			t.Fatalf("a vanished item was deleted: %v", err)
		}
		if key == gone {
			continue
		}
		if item.State != store.StateMissingUpstream || !item.NeedsReview {
			t.Errorf("%s is %q, review=%v, want missing_upstream", key, item.State, item.NeedsReview)
		}
	}
}

func TestRunsAreRecorded(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "bytes"})

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	runs, err := h.store.RecentRuns(10)
	if err != nil {
		t.Fatalf("listing runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != store.OutcomeOK || runs[0].Downloaded != 1 {
		t.Fatalf("the run was recorded as %+v", runs)
	}
	if runs[0].FinishedAt.IsZero() {
		t.Error("a finished run has no finish time")
	}
}

func TestStalePartsAreDiscardedButResumableOnesKept(t *testing.T) {
	temp := t.TempDir()
	for _, name := range []string{"wanted.part", "abandoned.part", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(temp, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	discarded, err := discardStaleParts(temp, map[string]bool{"wanted": true})
	if err != nil {
		t.Fatalf("discarding stale parts: %v", err)
	}
	if discarded != 1 {
		t.Errorf("discarded %d files, want 1", discarded)
	}

	for _, name := range []string{"wanted.part", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(temp, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(temp, "abandoned.part")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the abandoned partial survived")
	}
}

// Google's filenames and media keys are remote input that become path components.
func TestPoolPathsCannotEscapeThePool(t *testing.T) {
	cases := []store.MediaItem{
		{MediaKey: "../../etc", Filename: "passwd"},
		{MediaKey: "key", Filename: "../../../etc/passwd"},
		{MediaKey: "key", Filename: `..\..\windows\system32`},
		{MediaKey: "key", Filename: "/absolute"},
	}

	// A literal ".." inside a filename is harmless; only a whole path component of ".." walks
	// upwards. The invariant is therefore about components, not substrings.
	for _, item := range cases {
		path := filepath.Clean(poolPath("/pool", item))
		if !strings.HasPrefix(path, "/pool/") {
			t.Errorf("%+v produced %q, which escapes the pool", item, path)
		}
		for _, component := range strings.Split(path, string(filepath.Separator)) {
			if component == ".." {
				t.Errorf("%+v produced %q, which walks out of the pool", item, path)
			}
		}
	}
}

func TestItemsWithoutADateAreStillFiled(t *testing.T) {
	path := poolPath("/pool", store.MediaItem{MediaKey: "abcdefghijklmnop", Filename: "x.jpg"})
	if !strings.Contains(path, unknownDateDir) {
		t.Errorf("a dateless item was filed at %q", path)
	}
	if !strings.Contains(filepath.Base(path), "abcdefghijkl_") {
		t.Errorf("the filename is not prefixed by the media key: %q", filepath.Base(path))
	}
}

func TestBackoffObeysRetryAfterAndOtherwiseGrows(t *testing.T) {
	backoff := Backoff{Base: time.Second, Max: time.Minute}

	if delay := backoff.Delay(0, 42*time.Second); delay != 42*time.Second {
		t.Errorf("Retry-After produced a %s delay, want exactly 42s", delay)
	}

	first := backoff.Delay(0, 0)
	third := backoff.Delay(3, 0)
	if third <= first {
		t.Errorf("the delay did not grow: %s then %s", first, third)
	}
	if capped := backoff.Delay(40, 0); capped < backoff.Max || capped > 2*backoff.Max {
		t.Errorf("a huge attempt count produced %s, want it capped near %s", capped, backoff.Max)
	}
}

func TestRetryClassification(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"server error":    {&gphotos.HTTPError{StatusCode: 503}, true},
		"rate limited":    {&gphotos.HTTPError{StatusCode: 429}, true},
		"bad request":     {&gphotos.HTTPError{StatusCode: 400}, false},
		"truncated body":  {gphotos.ErrTruncated, true},
		"expired url":     {gphotos.ErrSignedURLRejected, true},
		"session died":    {gphotos.ErrSessionRejected, false},
		"protocol drift":  {gphotos.ErrProtocolDrift, false},
		"unknown host":    {gphotos.ErrUnknownContentHost, false},
		"run cancelled":   {context.Canceled, false},
		"deadline passed": {context.DeadlineExceeded, false},
		"wrapped 503":     {fmt.Errorf("fetching: %w", &gphotos.HTTPError{StatusCode: 500}), true},
		"unclassified":    {errors.New("connection reset"), true},
	}

	for name, test := range cases {
		if got := Retryable(test.err); got != test.want {
			t.Errorf("%s: Retryable = %v, want %v", name, got, test.want)
		}
	}
}

func TestBreakerTripsOnlyOnConsecutiveFailures(t *testing.T) {
	breaker := NewBreaker(3)

	for range 2 {
		if breaker.Fail(errors.New("boom")) {
			t.Fatal("the breaker tripped early")
		}
	}
	breaker.Succeed()

	for range 2 {
		if breaker.Fail(errors.New("boom")) {
			t.Fatal("a success did not reset the breaker")
		}
	}
	if !breaker.Fail(errors.New("boom")) {
		t.Fatal("the breaker did not trip at its threshold")
	}
	if breaker.Fail(errors.New("boom")) {
		t.Error("the breaker reported tripping twice")
	}
	if breaker.Tripped() == nil {
		t.Error("a tripped breaker reports no cause")
	}
}

// A listing gives no filenames — the name arrives with the download. Standing the media key in
// for one put an identifier everywhere a name is shown: grid captions, checkbox labels, the
// viewer, and the card that says what is downloading.
func TestAListedItemIsStoredWithNoNameOfItsOwn(t *testing.T) {
	listed := toStoreItem(gphotos.MediaItem{MediaKey: "key-a"})

	if listed.Filename != "" {
		t.Errorf("an item nothing has downloaded yet is named %q", listed.Filename)
	}
}

// The album listing alone is not the library. Measured live on 2026-08-10 it omitted 18 albums
// shared with the account — 3714 photos — that only the second listing returns, so a refresh
// that reads one listing silently backs up less than the user can see in Google Photos.
func TestRefreshReadsBothListings(t *testing.T) {
	harness := newHarness(t, store.SyncNone, map[string]string{})
	harness.source.albums = []gphotos.Album{
		{ID: "mine", Title: "Holiday", Kind: gphotos.AlbumOwned},
		{ID: "loose", Kind: gphotos.AlbumBundle},
	}
	harness.source.sharedAlbums = []gphotos.Album{
		{ID: "mine", Title: "Holiday"},
		{ID: "theirs", Title: "kanari"},
	}

	if err := harness.syncer.RefreshAlbums(t.Context()); err != nil {
		t.Fatalf("refreshing albums: %v", err)
	}

	albums, err := harness.store.Albums()
	if err != nil {
		t.Fatalf("reading albums back: %v", err)
	}

	kinds := map[string]store.AlbumKind{}
	titles := map[string]string{}
	for _, album := range albums {
		kinds[album.ID], titles[album.ID] = album.Kind, album.Title
	}

	want := map[string]store.AlbumKind{
		"mine": store.AlbumOwned, "loose": store.AlbumBundle, "theirs": store.AlbumShared,
	}
	for id, kind := range want {
		if kinds[id] != kind {
			t.Errorf("album %q recorded as %q, want %q", id, kinds[id], kind)
		}
	}
	if titles["theirs"] != "kanari" {
		t.Errorf("the album only the shared listing knows has title %q", titles["theirs"])
	}
}

// An album in both listings must not have its kind overwritten by the second one: the shared
// listing returns the user's own albums too, indistinguishable from the ones shared with them.
func TestAnAlbumInBothListingsStaysTheUsersOwn(t *testing.T) {
	harness := newHarness(t, store.SyncNone, map[string]string{})
	harness.source.albums = []gphotos.Album{{ID: "mine", Title: "Holiday", Kind: gphotos.AlbumOwned}}
	harness.source.sharedAlbums = []gphotos.Album{{ID: "mine", Title: "Holiday"}}

	if err := harness.syncer.RefreshAlbums(t.Context()); err != nil {
		t.Fatalf("refreshing albums: %v", err)
	}

	album, err := harness.store.Album("mine")
	if err != nil {
		t.Fatalf("reading the album back: %v", err)
	}
	if album.Kind != store.AlbumOwned {
		t.Errorf("an album in both listings ended up as %q", album.Kind)
	}
}

// Only this layer holds both the listing and the session that says who the reader is, so this
// is where "Péter shared these" becomes "you shared these" — the difference between a set the
// user sent out and one that arrived, on rows that have no other name.
func TestTheAccountHolderIsRecognisedAmongTheOwners(t *testing.T) {
	harness := newHarness(t, store.SyncNone, map[string]string{})
	harness.source.accountID = "me"
	harness.source.albums = []gphotos.Album{
		{ID: "mine", Kind: gphotos.AlbumBundle, Owner: gphotos.Person{ID: "me", Name: "Me Myself"}},
		{ID: "theirs", Kind: gphotos.AlbumBundle, Owner: gphotos.Person{ID: "you", Name: "Sam Sharer"}},
	}

	if err := harness.syncer.RefreshAlbums(t.Context()); err != nil {
		t.Fatalf("refreshing albums: %v", err)
	}

	mine, err := harness.store.Album("mine")
	if err != nil {
		t.Fatalf("reading the album back: %v", err)
	}
	if !mine.OwnerIsAccount || mine.OwnerName != "Me Myself" {
		t.Errorf("the account holder's own bundle recorded owner %q, mine=%v",
			mine.OwnerName, mine.OwnerIsAccount)
	}

	theirs, err := harness.store.Album("theirs")
	if err != nil {
		t.Fatalf("reading the album back: %v", err)
	}
	if theirs.OwnerIsAccount || theirs.OwnerName != "Sam Sharer" {
		t.Errorf("someone else's bundle recorded owner %q, mine=%v",
			theirs.OwnerName, theirs.OwnerIsAccount)
	}
}

// A page shell that stops carrying the account id must not turn every album in the library
// into one the user shared themselves. Silence is the safe answer: the owner is still named.
func TestAnUnknownAccountClaimsNothing(t *testing.T) {
	harness := newHarness(t, store.SyncNone, map[string]string{})
	harness.source.albums = []gphotos.Album{
		{ID: "bundle", Kind: gphotos.AlbumBundle, Owner: gphotos.Person{Name: "Sam Sharer"}},
	}

	if err := harness.syncer.RefreshAlbums(t.Context()); err != nil {
		t.Fatalf("refreshing albums: %v", err)
	}

	album, err := harness.store.Album("bundle")
	if err != nil {
		t.Fatalf("reading the album back: %v", err)
	}
	if album.OwnerIsAccount {
		t.Error("an unknown account id was taken to mean the user owns everything")
	}
}

// libraryHarness follows the library over a two-page timeline whose photos run from the newest
// day backwards, which is the order Google serves and the order the date bound relies on.
func libraryHarness(t *testing.T, since time.Time) *harness {
	t.Helper()

	h := newHarness(t, store.SyncNone, map[string]string{})
	h.source.timeline = [][]gphotos.MediaItem{
		{
			timelineItem("recent-a", time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)),
			timelineItem("recent-b", time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)),
		},
		{
			timelineItem("old-a", time.Date(2019, 3, 1, 12, 0, 0, 0, time.UTC)),
			timelineItem("old-b", time.Date(2019, 2, 1, 12, 0, 0, 0, time.UTC)),
		},
	}
	for _, page := range h.source.timeline {
		for _, item := range page {
			h.source.bodies[item.MediaKey] = []byte("bytes of " + item.MediaKey)
		}
	}

	if err := h.store.SetLibrary(store.SyncAll, since); err != nil {
		t.Fatalf("following the library: %v", err)
	}
	return h
}

func timelineItem(mediaKey string, capturedAt time.Time) gphotos.MediaItem {
	return gphotos.MediaItem{MediaKey: mediaKey, CapturedAt: capturedAt, Width: 100, Height: 100}
}

// The library is where every photo that is in no album lives, so following it has to reach
// items no album listing ever mentions.
func TestFollowingTheLibraryBacksUpPhotosInNoAlbum(t *testing.T) {
	h := libraryHarness(t, time.Time{})

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if report.Listed != 4 || report.Downloaded != 4 {
		t.Fatalf("the report is %+v, want 4 listed and 4 downloaded", report)
	}
	if pages := h.source.timelinePagesServed(); pages != 2 {
		t.Errorf("the walk asked for %d timeline pages, want both", pages)
	}
}

// The date is the only dial between "back up my recent photos" and "back up a hundred thousand
// items", so it has to stop the listing as well as the downloading.
func TestTheLibraryDateBoundStopsTheWalk(t *testing.T) {
	h := libraryHarness(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if report.Listed != 2 || report.Downloaded != 2 {
		t.Fatalf("the report is %+v, want only the two items after the bound", report)
	}
	if pages := h.source.timelinePagesServed(); pages != 2 {
		t.Errorf("the walk asked for %d timeline pages; it should read the page that crosses "+
			"the bound and then stop", pages)
	}

	if _, err := h.store.Item("old-a"); err == nil {
		t.Error("an item older than the bound was recorded anyway")
	}
}

// The download rpc wants an album id for its permission check and refuses an empty string; a
// library photo has no album to name, so it must be asked for with none at all.
func TestALibraryPhotoIsDownloadedWithoutAnAlbum(t *testing.T) {
	h := libraryHarness(t, time.Time{})

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	for _, albumID := range h.source.albumsRequested() {
		if albumID != "" {
			t.Fatalf("a library photo was requested against album %q", albumID)
		}
	}
}

// A photo in no album is reachable only through the timeline, so the timeline walk is the only
// thing that can ever notice it has been deleted. Until this was written the library was walked
// but never reconciled, and a library photo deleted from Google stayed "downloaded" for ever.
func TestAPhotoDeletedFromTheLibraryIsWrittenOffAndKeptOnDisk(t *testing.T) {
	h := libraryHarness(t, time.Time{})
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the first run: %v", err)
	}

	h.source.timeline[0] = h.source.timeline[0][:1]
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the second run: %v", err)
	}

	deleted, err := h.store.Item("recent-b")
	if err != nil {
		t.Fatalf("reading the deleted item back: %v", err)
	}
	if deleted.State != store.StateMissingUpstream || !deleted.NeedsReview {
		t.Errorf("the deleted photo is %q, review=%v", deleted.State, deleted.NeedsReview)
	}
	if _, err := os.Stat(deleted.LocalPath); err != nil {
		t.Errorf("the backup of a deleted photo went with it: %v", err)
	}

	kept, err := h.store.Item("recent-a")
	if err != nil {
		t.Fatalf("reading the surviving item back: %v", err)
	}
	if kept.State != store.StateDone {
		t.Errorf("a photo the walk still lists is %q", kept.State)
	}

	// The write-off is only useful if the user is asked about it, and the queue groups by album:
	// for a photo in no album, the library is the only group it can appear under.
	groups, err := h.store.ItemsNeedingReview(store.StateMissingUpstream)
	if err != nil {
		t.Fatalf("reading the review queue: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Items) != 1 || groups[0].Items[0].MediaKey != "recent-b" {
		t.Errorf("the review queue holds %+v, want the deleted photo under the library", groups)
	}
}

// The bound is the user saying "do not look further back than this", not Google saying those
// photos are gone. A walk that stops there has seen nothing of what lies below it.
func TestAWalkThatStopsAtTheBoundWritesOffNothingBelowIt(t *testing.T) {
	h := libraryHarness(t, time.Time{})
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the unbounded run: %v", err)
	}

	if err := h.store.SetLibrary(store.SyncAll, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("moving the bound: %v", err)
	}
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the bounded run: %v", err)
	}

	for _, mediaKey := range []string{"old-a", "old-b"} {
		item, err := h.store.Item(mediaKey)
		if err != nil {
			t.Fatalf("reading %s back: %v", mediaKey, err)
		}
		if item.State == store.StateMissingUpstream {
			t.Errorf("%s was written off as gone from Google for being older than the bound", mediaKey)
		}
	}
}

// Capture times carry the camera's timezone, so around the bound the timeline's order wobbles by
// the better part of a day: an item captured just above it can sit on the page after the one the
// walk stopped on. Reconciling right up to the bound would write off a photo that is still there.
func TestAnItemCapturedAtTheBoundSurvivesAWalkThatStoppedThere(t *testing.T) {
	bound := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := libraryHarness(t, bound)
	// The walk stops on the page that crosses the bound, so the archive below it goes unasked
	// for — which is exactly where an item that wobbled out of order would be sitting.
	h.source.timeline = [][]gphotos.MediaItem{
		h.source.timeline[0],
		{timelineItem("at-the-bound", bound.Add(6*time.Hour)), h.source.timeline[1][0]},
		{h.source.timeline[1][1]},
	}
	h.source.bodies["at-the-bound"] = []byte("bytes of at-the-bound")

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	if _, err := h.store.Item("at-the-bound"); err != nil {
		t.Fatalf("the item at the bound was never listed: %v", err)
	}

	h.source.timeline[1] = h.source.timeline[1][1:]
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the second run: %v", err)
	}

	item, err := h.store.Item("at-the-bound")
	if err != nil {
		t.Fatalf("reading the item at the bound back: %v", err)
	}
	if item.State == store.StateMissingUpstream {
		t.Error("an item captured within the wobble of the bound was written off as gone")
	}
}

// A timeline that answers with nothing is a failure wearing a success's clothes. Believed, it
// would write off the user's entire library in one run.
func TestAnEmptyTimelineWritesOffNothing(t *testing.T) {
	h := libraryHarness(t, time.Time{})
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the first run: %v", err)
	}

	h.source.timeline = nil
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the empty run: %v", err)
	}

	for _, mediaKey := range []string{"recent-a", "recent-b", "old-a", "old-b"} {
		item, err := h.store.Item(mediaKey)
		if err != nil {
			t.Fatalf("reading %s back: %v", mediaKey, err)
		}
		if item.State == store.StateMissingUpstream {
			t.Errorf("%s was written off on the word of an empty timeline", mediaKey)
		}
	}
}

// An unfollowed library must stay untouched: a fresh install downloads nothing until asked,
// and the library is the one row where "everything" means a hundred thousand items.
func TestAnUnfollowedLibraryIsNeverWalked(t *testing.T) {
	h := libraryHarness(t, time.Time{})
	if err := h.store.SetLibrary(store.SyncNone, time.Time{}); err != nil {
		t.Fatalf("unfollowing the library: %v", err)
	}

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if pages := h.source.timelinePagesServed(); pages != 0 {
		t.Errorf("the timeline was walked %d times for an unfollowed library", pages)
	}
}

// The review queue exists because a 'picked' album is one the user curates by hand: an item
// Google adds to it later was chosen by nobody, so it is recorded and held rather than fetched.
// DESIGN.md §4 has said so all along; until this was written nothing set the flag.
func TestAnItemArrivingInAPickedAlbumIsHeldForReview(t *testing.T) {
	h := newHarness(t, store.SyncPicked, map[string]string{"key-1": "one", "key-2": "two"})

	if _, err := h.syncer.ListAlbum(t.Context(), "album-1"); err != nil {
		t.Fatalf("the first listing: %v", err)
	}
	if waiting := reviewKeys(t, h); len(waiting) != 0 {
		t.Fatalf("the first walk of an album queued %v for review; it is the baseline", waiting)
	}

	h.source.items["album-1"] = append(h.source.items["album-1"], gphotos.MediaItem{
		MediaKey:   "key-3",
		CapturedAt: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC),
		Width:      100, Height: 100,
	})
	if _, err := h.syncer.ListAlbum(t.Context(), "album-1"); err != nil {
		t.Fatalf("the second listing: %v", err)
	}

	waiting := reviewKeys(t, h)
	if len(waiting) != 1 || waiting[0] != "key-3" {
		t.Fatalf("the review queue holds %v, want only the item that arrived", waiting)
	}
}

// Holding an item back is only meaningful if it also stays out of the download set. An item
// that were flagged and fetched anyway would make the queue a notification, not a decision.
func TestAnItemHeldForReviewIsNotDownloaded(t *testing.T) {
	h := newHarness(t, store.SyncPicked, map[string]string{"key-1": "one"})

	if _, err := h.syncer.ListAlbum(t.Context(), "album-1"); err != nil {
		t.Fatalf("the first listing: %v", err)
	}
	h.source.items["album-1"] = append(h.source.items["album-1"], gphotos.MediaItem{
		MediaKey:   "key-2",
		CapturedAt: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC),
		Width:      100, Height: 100,
	})
	h.source.bodies["key-2"] = []byte("two")

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	item, err := h.store.Item("key-2")
	if err != nil {
		t.Fatalf("reading the held item: %v", err)
	}
	if item.State == store.StateDone {
		t.Error("an item waiting for a decision was downloaded anyway")
	}
}

// An album followed 'all' asks nobody: that is what following it means. A review queue that
// filled up for followed albums would make the normal case the noisy one.
func TestAnItemArrivingInAFollowedAlbumIsNeverQueued(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})

	if _, err := h.syncer.ListAlbum(t.Context(), "album-1"); err != nil {
		t.Fatalf("the first listing: %v", err)
	}
	h.source.items["album-1"] = append(h.source.items["album-1"], gphotos.MediaItem{
		MediaKey:   "key-2",
		CapturedAt: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC),
		Width:      100, Height: 100,
	})
	if _, err := h.syncer.ListAlbum(t.Context(), "album-1"); err != nil {
		t.Fatalf("the second listing: %v", err)
	}

	if waiting := reviewKeys(t, h); len(waiting) != 0 {
		t.Errorf("a followed album queued %v for review", waiting)
	}
}

func reviewKeys(t *testing.T, h *harness) []string {
	t.Helper()

	groups, err := h.store.ItemsNeedingReview(store.StateDiscovered)
	if err != nil {
		t.Fatalf("reading the review queue: %v", err)
	}

	var keys []string
	for _, group := range groups {
		for _, item := range group.Items {
			keys = append(keys, item.MediaKey)
		}
	}
	return keys
}

// A full disk must end the run, not each item in it. Left to the filesystem, the end of the
// disk arrives as thousands of write failures, and every one of them is charged to an item's
// retry budget — so items that were never the problem become permanently failed.
func TestARunStopsBeforeEatingIntoTheFreeSpaceFloor(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one", "key-2": "two"})

	free, err := FreeBytes(h.pool)
	if err != nil {
		t.Fatalf("measuring the scratch filesystem: %v", err)
	}
	h.syncer.options.MinFreeBytes = int64(free) + (1 << 30)

	report, err := h.syncer.Run(t.Context())
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("the run ended with %v, want ErrDiskFull", err)
	}
	if report.Downloaded != 0 {
		t.Errorf("%d items were downloaded past the floor", report.Downloaded)
	}
	if report.Failed != 0 {
		t.Errorf("stopping for space charged %d items with a failure", report.Failed)
	}
	if report.Outcome != store.OutcomeError {
		t.Errorf("the run recorded outcome %q, want error", report.Outcome)
	}
}

// The floor must not stop a run that has room, and an unset floor must not stop one at all.
func TestARunWithRoomIsUnaffectedByTheFloor(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	h.syncer.options.MinFreeBytes = 1

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if files := h.poolFiles(t); len(files) != 1 {
		t.Errorf("the pool holds %v, want the one item", files)
	}
}

// The store's word that a file exists is not evidence that it does. A file lost to a bad disk,
// a careless rsync or a half-restored snapshot must come back, or the backup quietly has a hole
// in it that the page reporting everything backed up would never show.
func TestARunFetchesAgainWhatVanishedFromDisk(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	files := h.poolFiles(t)
	if len(files) != 1 {
		t.Fatalf("the first run left %v in the pool, want one file", files)
	}

	item, err := h.store.Item("key-1")
	if err != nil {
		t.Fatalf("reading the item: %v", err)
	}
	if err := os.Remove(item.LocalPath); err != nil {
		t.Fatalf("removing the backed-up file: %v", err)
	}

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("the second run: %v", err)
	}
	if report.Downloaded != 1 {
		t.Errorf("the second run downloaded %d items, want the one that vanished", report.Downloaded)
	}
	if files := h.poolFiles(t); len(files) != 1 {
		t.Errorf("the pool holds %v, want the file back", files)
	}
}

// The pass must not mistake "still here" for "gone", or every run would re-fetch the library.
func TestARunLeavesFilesThatAreStillThereAlone(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	before := h.source.calls()

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("the second run: %v", err)
	}
	if report.Downloaded != 0 {
		t.Errorf("the second run re-downloaded %d items that were still on disk", report.Downloaded)
	}
	if after := h.source.calls(); after != before {
		t.Errorf("the second run made %d more download calls", after-before)
	}
}

// seedBacklog puts an item in the store as a previous run would have left it: known, linked to
// a followed album and not yet downloaded. It is what makes a run's work list non-empty before
// that run has listed anything.
func seedBacklog(t *testing.T, h *harness, keys ...string) {
	t.Helper()

	listedAt := time.Now()
	for _, key := range keys {
		if err := h.store.UpsertItem(store.MediaItem{MediaKey: key, Filename: key}, listedAt); err != nil {
			t.Fatalf("seeding the backlog item %s: %v", key, err)
		}
		if err := h.store.LinkItemToAlbum("album-1", key, listedAt); err != nil {
			t.Fatalf("linking the backlog item %s: %v", key, err)
		}
	}
}

// The point of the whole exercise: a run must not sit on its hands through the listing. On a
// real library that phase is the better part of an hour, and everything owed from the last run
// could have been fetched throughout it.
func TestDownloadingStartsBeforeTheListingHasFinished(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "already owed"})
	seedBacklog(t, h, "key-a")

	// The listing does not return until something has been downloaded. If a run still listed
	// everything first this would time out rather than deadlock, so the run always completes and
	// the failure is a message rather than a hung suite.
	downloaded := make(chan struct{})
	var once sync.Once
	h.source.onDownload = func() { once.Do(func() { close(downloaded) }) }
	h.source.beforeAlbumItems = func() {
		select {
		case <-downloaded:
		case <-time.After(10 * time.Second):
			t.Error("the listing finished with nothing downloaded: the two halves are still sequential")
		}
	}

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if report.Downloaded != 1 {
		t.Errorf("the run downloaded %d items, want 1", report.Downloaded)
	}
}

// A listing that fails now happens alongside downloads that did not, so the run has two answers
// to reconcile. The work that landed is kept and the failure is still the run's outcome.
func TestAFailedListingStillKeepsWhatTheWorkersFetched(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-a": "already owed"})
	seedBacklog(t, h, "key-a")

	refused := errors.New("google refused the album listing")
	h.source.albumItemsErr = refused

	report, err := h.syncer.Run(t.Context())
	if !errors.Is(err, refused) {
		t.Fatalf("the run ended with %v, want the listing failure", err)
	}
	if report.Downloaded != 1 {
		t.Errorf("the run downloaded %d items, want the backlog item to have landed anyway", report.Downloaded)
	}
	if report.Outcome != store.OutcomeError {
		t.Errorf("the run recorded outcome %q, want error", report.Outcome)
	}
}

// stopListing takes an album out of what Google reports without unfollowing it, which is the
// state a deleted album or a withdrawn share leaves behind: the store still has the row and the
// user's instruction to back it up, and no listing mentions it again.
func stopListing(h *harness, albumID string) {
	kept := make([]gphotos.Album, 0, len(h.source.albums))
	for _, album := range h.source.albums {
		if album.ID != albumID {
			kept = append(kept, album)
		}
	}
	h.source.albums = kept
}

// Following outlives listing: nothing clears sync_mode when an album is deleted or a share is
// withdrawn, so the id was asked for on every run forever and answered with something that is
// not a listing. The refresh at the top of the run already knows better.
func TestAnAlbumGoogleNoLongerListsIsNotWalked(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	followAlbum(t, h, "album-2", "Zzz Last", "key-2")
	stopListing(h, "album-1")

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if slices.Contains(h.source.albumsWalked(), "album-1") {
		t.Error("an album Google no longer lists was asked for anyway")
	}
	if len(report.UnlistedAlbums) != 1 || !strings.Contains(report.UnlistedAlbums[0], "album-1") {
		t.Errorf("the run reports %v unlisted, want the album Google dropped", report.UnlistedAlbums)
	}
	if report.Outcome != store.OutcomePartial {
		t.Errorf("a run that left an album unwalked recorded outcome %q, want partial", report.Outcome)
	}
}

// Not walking an album must not be mistaken for walking it and finding it empty. The links are
// the album view and the user's selections; writing them off because Google stopped mentioning
// the album would destroy the record of what was in it.
func TestAnAlbumGoogleNoLongerListsKeepsItsContents(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	followAlbum(t, h, "album-2", "Zzz Last", "key-2")

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	stopListing(h, "album-1")
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the second run: %v", err)
	}

	members, err := h.store.MembersOf("album-1")
	if err != nil {
		t.Fatalf("reading the album: %v", err)
	}
	if !members["key-1"] {
		t.Error("an album Google stopped listing lost the items it held")
	}
}

// One album can genuinely go. Every album going at once between two nightly runs is not four
// hundred deletions, it is a listing this run should not have believed — and believed, it would
// leave the user with a backup that had quietly stopped walking anything.
func TestARunWhereGoogleListedNoFollowedAlbumFails(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	followAlbum(t, h, "album-2", "Zzz Last", "key-2")
	h.source.albums = nil

	report, err := h.syncer.Run(t.Context())
	if err == nil {
		t.Fatal("a run that was offered none of its albums reported success")
	}
	if report.Outcome == store.OutcomeOK || report.Outcome == store.OutcomePartial {
		t.Errorf("a run that was offered none of its albums recorded outcome %q", report.Outcome)
	}
}

// An album Google has dropped stays unwalked on every run from now on, so the run page has to say
// what to do about it rather than showing an unexplained partial for ever.
func TestTheRunPageSaysToUnfollowAnAlbumGoogleDropped(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	followAlbum(t, h, "album-2", "Zzz Last", "key-2")
	stopListing(h, "album-1")

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	runs, err := h.store.RecentRuns(1)
	if err != nil || len(runs) == 0 {
		t.Fatalf("reading the run: %v", err)
	}
	for _, want := range []string{"no longer listed by Google", "everything it held is kept",
		"unfollowing it", "album-1"} {
		if !strings.Contains(runs[0].Error, want) {
			t.Errorf("the recorded run does not mention %q: %q", want, runs[0].Error)
		}
	}
}

// The two rules that keep stepping over albums honest, stated on their own. Drift is judged by
// whether anything decoded at all; an album Google stopped naming is judged by whether Google
// named anything at all. Neither is a count or a threshold, because a count cannot tell four
// hundred deletions from one broken listing — both look like "all of them".
func TestAWalkFailsOnlyWhenTheWholeListingIsInDoubt(t *testing.T) {
	drift := fmt.Errorf("%w: nothing decoded", gphotos.ErrProtocolDrift)
	cases := map[string]struct {
		walk     albumWalk
		wantFail bool
	}{
		"everything decoded":      {walk: albumWalk{walked: 3}},
		"one album of many":       {walk: albumWalk{walked: 2, skipped: []string{"a"}, drifted: drift}},
		"many albums among a few": {walk: albumWalk{walked: 1, skipped: make([]string, 400), drifted: drift}},
		"the only album there was": {
			walk: albumWalk{skipped: []string{"a"}, drifted: drift}, wantFail: true,
		},
		"every album drifted": {
			walk: albumWalk{skipped: []string{"a", "b"}, drifted: drift}, wantFail: true,
		},
		"one album gone from a listing that named others": {
			walk: albumWalk{walked: 2, unlisted: []string{"a"}},
		},
		"the only album gone from a listing that named others": {
			walk: albumWalk{unlisted: []string{"a"}},
		},
		"every album gone from a listing that named nothing": {
			walk: albumWalk{unlisted: []string{"a", "b"}, listingWasEmpty: true}, wantFail: true,
		},
		"nothing followed at all": {walk: albumWalk{listingWasEmpty: true}},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			err := test.walk.verdict()
			if failed := err != nil; failed != test.wantFail {
				t.Errorf("a walk of %d walked, %d skipped and %d unlisted returned %v, want a failure: %t",
					test.walk.walked, len(test.walk.skipped), len(test.walk.unlisted), err, test.wantFail)
			}
		})
	}
}

// The case that decides the rule: someone who follows one album and deletes it in Google Photos
// must not lose their library backup over it. Google still lists their other albums, so the one
// that went is a deletion — reported, unfollowed, and nothing else disturbed.
func TestDeletingTheOnlyFollowedAlbumDoesNotStopTheLibraryWalk(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	h.source.albums = append(h.source.albums, gphotos.Album{ID: "album-2", Title: "Not followed"})
	h.source.timeline = [][]gphotos.MediaItem{{{
		MediaKey:   "key-loose",
		CapturedAt: time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
		Width:      100, Height: 100,
	}}}
	h.source.bodies["key-loose"] = []byte("loose")
	if err := h.store.SetLibrary(store.SyncAll, time.Time{}); err != nil {
		t.Fatalf("following the library: %v", err)
	}
	stopListing(h, "album-1")

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("deleting the only followed album ended the run: %v", err)
	}
	if h.source.timelinePagesServed() == 0 {
		t.Error("the library was never walked")
	}
	if len(report.UnlistedAlbums) != 1 {
		t.Errorf("the run reports %v unlisted, want the deleted album", report.UnlistedAlbums)
	}
}

// followAlbum adds a second album to a harness that starts with one, so a test can ask what
// happens to the albums behind the one that fails. Walk order is favourites, then title, then id.
func followAlbum(t *testing.T, h *harness, albumID, title string, keys ...string) {
	t.Helper()

	for _, key := range keys {
		h.source.items[albumID] = append(h.source.items[albumID], gphotos.MediaItem{
			MediaKey:   key,
			CapturedAt: time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
			Width:      100, Height: 100,
		})
		h.source.bodies[key] = []byte(key)
	}
	h.source.albums = append(h.source.albums, gphotos.Album{ID: albumID, Title: title, ItemCount: len(keys)})

	if err := h.store.UpsertAlbum(store.Album{ID: albumID, Title: title}, time.Now()); err != nil {
		t.Fatalf("seeding %s: %v", albumID, err)
	}
	if err := h.store.SetAlbumSyncMode(albumID, store.SyncAll); err != nil {
		t.Fatalf("following %s: %v", albumID, err)
	}
}

// One album Google answers strangely used to cost the whole nightly backup: every album behind
// it in the walk order and the library timeline behind them all went unread, every run, until
// somebody noticed. Stepping over it is the difference between a broken album and a broken backup.
func TestAnAlbumThatWillNotDecodeIsSteppedOverRatherThanEndingTheRun(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	followAlbum(t, h, "album-2", "Zzz Last", "key-2")
	h.source.albumItemsErrs["album-1"] = fmt.Errorf("%w: wanted an array of media items", gphotos.ErrProtocolDrift)

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("one undecodable album ended the run: %v", err)
	}
	if report.Listed != 1 || report.Downloaded != 1 {
		t.Errorf("the run listed %d and downloaded %d, want the album behind the bad one to have been walked",
			report.Listed, report.Downloaded)
	}
	if report.Outcome != store.OutcomePartial {
		t.Errorf("a run that skipped an album recorded outcome %q, want partial", report.Outcome)
	}
	if len(report.SkippedAlbums) != 1 || !strings.Contains(report.SkippedAlbums[0], "album-1") {
		t.Errorf("the run reports %v skipped, want the album that would not decode", report.SkippedAlbums)
	}
}

// Stepping over an album is only safe because a partial listing never reaches reconciliation.
// If it did, an album Google answered with nothing would have its whole contents written off as
// deleted — which is the loss the strict decoders were put there to prevent in the first place.
func TestASkippedAlbumKeepsEverythingItAlreadyHeld(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	followAlbum(t, h, "album-2", "Zzz Last", "key-2")

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	h.source.albumItemsErrs["album-1"] = fmt.Errorf("%w: wanted an array of media items", gphotos.ErrProtocolDrift)
	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the second run: %v", err)
	}

	members, err := h.store.MembersOf("album-1")
	if err != nil {
		t.Fatalf("reading the skipped album: %v", err)
	}
	if !members["key-1"] {
		t.Error("a skipped album lost the item it already held")
	}
}

// The whole point of a strict decoder is that a run reading nothing must not pass for a run over
// an empty account. Skipping restores a broken album; it must not quietly swallow broken decoders.
func TestARunWhereNoAlbumDecodedIsStillReportedAsDrift(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	followAlbum(t, h, "album-2", "Zzz Last", "key-2")
	drift := fmt.Errorf("%w: wanted an array of media items", gphotos.ErrProtocolDrift)
	h.source.albumItemsErrs["album-1"] = drift
	h.source.albumItemsErrs["album-2"] = drift

	report, err := h.syncer.Run(t.Context())
	if !errors.Is(err, gphotos.ErrProtocolDrift) {
		t.Fatalf("a run that decoded nothing ended with %v, want drift", err)
	}
	if report.Outcome != store.OutcomeDrift {
		t.Errorf("a run that decoded nothing recorded outcome %q, want drift", report.Outcome)
	}
}

// A signed-out session fails every album identically, so walking on would mean asking Google four
// hundred more times for something it has already refused — the opposite of what skipping is for.
func TestARejectedSessionStopsTheWalkInsteadOfSkipping(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	followAlbum(t, h, "album-2", "Zzz Last", "key-2")
	h.source.albumItemsErr = fmt.Errorf("%w: batchexecute answered with an HTML page", gphotos.ErrSessionRejected)

	report, err := h.syncer.Run(t.Context())
	if !errors.Is(err, gphotos.ErrSessionRejected) {
		t.Fatalf("the run ended with %v, want the rejected session", err)
	}
	if len(report.SkippedAlbums) != 0 {
		t.Errorf("a rejected session skipped %v, want the walk to have stopped", report.SkippedAlbums)
	}
	if walked := h.source.albumsWalked(); len(walked) != 1 {
		t.Errorf("a rejected session was asked for %d albums (%v), want the walk to stop at the first",
			len(walked), walked)
	}
}

// A skipped album finishes the run with no error at all, so unless it reaches the one column the
// run page reads, the album goes stale and nothing anywhere says which one or why.
func TestTheRunPageIsToldWhichAlbumsWereSkipped(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	followAlbum(t, h, "album-2", "Zzz Last", "key-2")
	h.source.albumItemsErrs["album-1"] = fmt.Errorf("%w: wanted an array of media items", gphotos.ErrProtocolDrift)

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("running the sync: %v", err)
	}

	runs, err := h.store.RecentRuns(1)
	if err != nil || len(runs) == 0 {
		t.Fatalf("reading the run: %v", err)
	}
	for _, want := range []string{"could not be listed", "Holiday", "album-1"} {
		if !strings.Contains(runs[0].Error, want) {
			t.Errorf("the recorded run does not mention %q: %q", want, runs[0].Error)
		}
	}
}

// A run walks hundreds of albums and stops at the first one that will not decode, so the failure
// has to say which. It also has to say what Google last claimed the album holds and when Google
// last mentioned it, because that is what tells an album gone from the account apart from an
// empty one and from real protocol drift.
func TestAFailedAlbumListingNamesTheAlbumItStoppedAt(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})

	lastSeen := time.Date(2026, 9, 9, 2, 11, 0, 0, time.UTC)
	seeded := store.Album{ID: "album-1", Title: "Holiday", Kind: store.AlbumShared, ItemCount: 24}
	if err := h.store.UpsertAlbum(seeded, lastSeen); err != nil {
		t.Fatalf("reseeding the album: %v", err)
	}
	h.source.albumItemsErr = errors.New("google refused the album listing")

	_, err := h.syncer.ListAlbum(t.Context(), "album-1")
	if err == nil {
		t.Fatal("a refused listing succeeded")
	}
	for _, want := range []string{"album-1", "Holiday", "shared", "24", lastSeen.Format(time.RFC3339)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}
}

// Two thirds of a real library's entries have no title of their own — bundles never do, and
// albums need not — so the failure has to name them by what they are rather than print an empty
// pair of quotes and leave the reader guessing which album broke.
func TestAnAlbumWithNoTitleIsNamedByWhatItIs(t *testing.T) {
	cases := map[store.Album]string{
		{Title: "Holiday", Kind: store.AlbumOwned}:  `the album "Holiday"`,
		{Title: "Holiday", Kind: store.AlbumShared}: `the shared album "Holiday"`,
		{Kind: store.AlbumOwned}:                    "the untitled album",
		{Kind: store.AlbumBundle}:                   "the untitled bundle of shared photos",
	}

	for album, want := range cases {
		if got := nameOf(album); got != want {
			t.Errorf("a %q album titled %q reads as %q, want %q", album.Kind, album.Title, got, want)
		}
	}
}

// Describing an album reads the store, which can itself fail — a row deleted between the walk
// starting and it failing, a database that has gone away. The id is the part worth keeping when
// nothing else can be read, because it is the part `gpb unfollow` takes.
func TestAnAlbumTooBrokenToDescribeIsStillNamed(t *testing.T) {
	h := newHarness(t, store.SyncAll, nil)

	described := h.syncer.describeAlbum("album-nobody-recorded")
	if !strings.Contains(described, "album-nobody-recorded") {
		t.Errorf("an undescribable album reads as %q, want its id", described)
	}
}

// An album with nothing in it is an ordinary album, not a failure. 0.1.3 reported one as drift
// on every run and stepped over it, which left a permanent `partial` on a library that was
// entirely backed up.
func TestAnEmptyAlbumIsWalkedLikeAnyOther(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one"})
	followAlbum(t, h, "album-empty", "2025. Karácsonyi kártyaparty")

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("a run over an empty album: %v", err)
	}
	if len(report.SkippedAlbums) > 0 {
		t.Errorf("an empty album was reported as unreadable: %v", report.SkippedAlbums)
	}
	if report.Outcome != store.OutcomeOK {
		t.Errorf("the run finished %q, want ok", report.Outcome)
	}
}

// The price of reading a page with no entries as an empty album is that entries moving to another
// slot would read the same way — and taken at face value it would empty every album on disk. The
// count Google gave in this run's own listing is the second opinion that stops it, and because
// every album with contents would hit this at once it ends the run rather than being stepped over.
func TestAnAlbumThatListsNothingGoogleSaysIsFullStopsTheRun(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one", "key-2": "two"})

	if _, err := h.syncer.Run(t.Context()); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	h.source.items["album-1"] = nil

	report, err := h.syncer.Run(t.Context())
	if !errors.Is(err, errAlbumCountContradicted) {
		t.Fatalf("a walk that listed none of two items ended with %v, want the count contradicted", err)
	}
	if !errors.Is(err, gphotos.ErrProtocolDrift) {
		t.Errorf("the contradiction is not reported as drift: %v", err)
	}
	if report.Outcome != store.OutcomeDrift {
		t.Errorf("the run finished %q, want drift", report.Outcome)
	}

	members, err := h.store.MembersOf("album-1")
	if err != nil {
		t.Fatalf("reading the album: %v", err)
	}
	if !members["key-1"] || !members["key-2"] {
		t.Errorf("the album was emptied on a listing that returned nothing: %v", members)
	}
}

// The feeder asks the store again as the listing turns up work, which is the only way a first
// run — with nothing owed when it starts — downloads anything at all.
func TestAFirstRunFetchesWhatItsOwnListingDiscovers(t *testing.T) {
	h := newHarness(t, store.SyncAll, map[string]string{"key-1": "one", "key-2": "two"})

	report, err := h.syncer.Run(t.Context())
	if err != nil {
		t.Fatalf("running the sync: %v", err)
	}
	if report.Listed != 2 || report.Downloaded != 2 {
		t.Errorf("the run listed %d and downloaded %d, want 2 and 2", report.Listed, report.Downloaded)
	}
}

// A shutdown must not look like a failure. The daemon decides whether a backup is owed from how
// the last one ended, so a run cut off by a restart being recorded as 'error' would leave it
// believing the day's backup had been attempted and answered.
func TestAShutdownIsRecordedAsInterruptedRatherThanFailed(t *testing.T) {
	for name, testCase := range map[string]struct {
		err  error
		want store.Outcome
	}{
		"asked to stop":     {context.Canceled, store.OutcomeInterrupted},
		"wrapped stop":      {fmt.Errorf("downloading key-1: %w", context.Canceled), store.OutcomeInterrupted},
		"ran out of time":   {context.DeadlineExceeded, store.OutcomeError},
		"google refused it": {gphotos.ErrSessionRejected, store.OutcomeAuthRequired},
		"nothing wrong":     {nil, store.OutcomeOK},
	} {
		t.Run(name, func(t *testing.T) {
			if got := outcomeOf(Report{}, testCase.err); got != testCase.want {
				t.Errorf("a run ending in %v was recorded as %q, want %q", testCase.err, got, testCase.want)
			}
		})
	}
}
