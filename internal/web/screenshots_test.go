package web

import (
	"context"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"gpb/internal/auth"
	"gpb/internal/config"
	"gpb/internal/homeassistant"
	"gpb/internal/store"
	"gpb/internal/syncer"
)

// The pictures in the README are drawn from an invented library: made-up album titles, made-up
// filenames, and thumbnails painted here rather than photographed anywhere. Nobody's album
// names or photographs belong in a public repository, and this way there is nothing to blur.
// The pages themselves are the real ones — the same handlers, templates and stylesheet the
// daemon serves.
//
// Everything the pages read is invented, down to the free space, so nothing here varies with the
// machine that runs it. The files still do not redraw byte-for-byte: albums.png and album.png
// differ in the cover images by a few values out of 255, because Chrome's downscaling is not
// bit-reproducible. Redraw them when a page has actually changed, and expect `git status` to
// show those two either way.
//
//	GPB_SCREENSHOTS=1 go test ./internal/web/ -run Screenshots
func TestScreenshotsForTheReadme(t *testing.T) {
	if os.Getenv("GPB_SCREENSHOTS") == "" {
		t.Skip("set GPB_SCREENSHOTS=1 to redraw the README's pictures")
	}

	server, _ := testServer(t)
	inventALibrary(t, server)

	site := httptest.NewServer(server.Handler())
	defer site.Close()

	browser, done := openBrowser(t)
	defer done()

	logIn(t, browser, site.URL)
	for _, shot := range []shot{
		{name: "overview", path: "/", waitFor: ".card"},
		{name: "albums", path: "/albums", waitFor: "table.albums"},
		// A grid page is the whole library laid out, sixty thousand pixels of it on the photo
		// page, and a photograph of all that is a photograph of nothing; one window of it is
		// what a reader sees.
		{name: "album", path: "/album/" + firstAlbumID, waitFor: ".grid", justTheWindow: true},
		{name: "photos", path: "/photos", waitFor: ".grid", justTheWindow: true},
		{name: "runs", path: "/runs", waitFor: "table.runs"},
		// The settings page ends with a card of read-only paths, one of which is the scratch
		// directory this test was handed. Photographing the form alone keeps a temporary path
		// out of the README without pretending the card is not there.
		{name: "settings", path: "/settings", waitFor: ".settings-form", justTheElement: true},
	} {
		capture(t, browser, site.URL+shot.path, shot)
	}
}

type shot struct {
	name, path, waitFor string
	justTheElement      bool
	justTheWindow       bool
}

const firstAlbumID = "album-ada"

func openBrowser(t *testing.T) (context.Context, func()) {
	t.Helper()

	allocator, releaseAllocator := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.WindowSize(1280, 900),
			chromedp.Flag("hide-scrollbars", true),
			chromedp.Flag("force-device-scale-factor", "2"),
		)...)

	browser, releaseBrowser := chromedp.NewContext(allocator)
	timed, cancelTimeout := context.WithTimeout(browser, 2*time.Minute)
	return timed, func() {
		cancelTimeout()
		releaseBrowser()
		releaseAllocator()
	}
}

func logIn(t *testing.T, browser context.Context, siteURL string) {
	t.Helper()

	if err := chromedp.Run(browser,
		chromedp.Navigate(siteURL+"/login"),
		chromedp.WaitVisible(`input[name="password"]`),
		chromedp.SendKeys(`input[name="password"]`, testPassword),
		chromedp.Click(`button[type="submit"]`),
		chromedp.WaitVisible(`.topbar`),
	); err != nil {
		t.Fatalf("logging the browser in: %v", err)
	}
}

func capture(t *testing.T, browser context.Context, url string, wanted shot) {
	t.Helper()

	var png []byte
	take := chromedp.FullScreenshot(&png, 100)
	if wanted.justTheElement {
		take = chromedp.Screenshot(wanted.waitFor, &png, chromedp.ByQuery)
	}
	if wanted.justTheWindow {
		take = chromedp.CaptureScreenshot(&png)
	}
	if err := chromedp.Run(browser,
		chromedp.Navigate(url),
		chromedp.WaitVisible(wanted.waitFor),
		// The live regions fill in over a websocket and the grid loads its images as they
		// scroll into view; a page caught mid-fill photographs as a page full of holes.
		chromedp.Sleep(2*time.Second),
		take,
	); err != nil {
		t.Fatalf("photographing %s: %v", url, err)
	}

	path := filepath.Join("..", "..", "docs", wanted.name+".png")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("making the docs directory: %v", err)
	}
	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	t.Logf("wrote %s (%d KB)", path, len(png)/1024)
}

// inventALibrary fills a scratch database with a plausible few years of photographs, and puts
// the parts of the daemon the pages ask about — the run in flight, the broker — into the state
// that shows what each page is for.
func inventALibrary(t *testing.T, server *Server) {
	t.Helper()

	now := time.Date(2026, 3, 14, 21, 40, 0, 0, time.Local)
	inventAlbums(t, server.store, now)
	inventRuns(t, server.store, now)

	server.images = paintedThumbnails{}
	server.auth = signedInGoogle{lastChecked: now.Add(-3 * time.Hour)}
	// The disk this test runs on has nothing to do with the library it is inventing, and its free
	// space changes between runs — which is enough to change the picture and nothing else.
	server.freeBytes = func(string) (uint64, error) { return 1_820_000_000_000, nil }
	server.mqtt = &fakeBridge{standing: homeassistant.Status{
		Broker:    "tcp://192.168.1.10:1883",
		Connected: true,
		Detail:    "publishing as gpb",
		Since:     now.Add(-70 * time.Hour),
	}}
	server.runs = &fakeRuns{
		activity: "sync",
		progress: syncer.Progress{
			Listed: 1_284, Downloaded: 311, Failed: 0, Owed: 946,
			Items: []syncer.InFlight{
				{MediaKey: "key-live-1", Filename: "PXL_20240712_183045.jpg", AlbumID: firstAlbumID,
					Written: 2_100_000, Total: 4_800_000},
				{MediaKey: "key-live-2", Filename: "MVI_2233.MOV", AlbumID: "album-iceland",
					Written: 71_000_000, Total: 522_000_000},
				{MediaKey: "key-live-3", Filename: "IMG_0431.HEIC", AlbumID: "album-attic",
					Written: 900_000, Total: 3_400_000},
			},
		},
	}

	cfg, err := config.Load(server.cfg.DataDir())
	if err != nil {
		t.Fatalf("reading the scratch config: %v", err)
	}
	// The invented runs all start at 02:40 because a scheduled backup does; the schedule on the
	// settings page is the one they follow, rather than the default nobody here ever set.
	cfg.Schedule.SyncAt = "02:40"
	cfg.Web.ExternalURL = "https://192.168.1.10:8090"
	cfg.Web.TLS.Mode = config.TLSSelfSigned
	cfg.MQTT.Broker = "192.168.1.10"
	cfg.MQTT.Username = "photos-sync"
	cfg.MQTT.Password = "not-the-real-one"
	if err := cfg.Save(); err != nil {
		t.Fatalf("writing the scratch config: %v", err)
	}
}

// signedInGoogle is a login that is simply working, which is the state every page is designed
// around and the one no test can reach: the real thing is a Chrome profile with this account's
// cookies in it.
type signedInGoogle struct{ lastChecked time.Time }

func (g signedInGoogle) Status() auth.Status {
	return auth.Status{
		State:       auth.StateOK,
		LastWarmup:  g.lastChecked,
		LastSuccess: g.lastChecked,
		CookieCount: 34,
		UserAgent:   "Mozilla/5.0 (X11; Linux x86_64) Chrome/126.0.0.0 Safari/537.36",
	}
}

func (g signedInGoogle) Warmup(context.Context) (*auth.Session, error) {
	return &auth.Session{}, nil
}

// invented is one album's worth of made-up history. Counts are what a library like this really
// looks like — a few enormous albums, a long tail of small ones, and a couple nobody follows.
type invented struct {
	id, title string
	kind      store.AlbumKind
	owner     string
	mine      bool
	items     int
	mode      store.SyncMode
	done      int
	pending   int
	failed    int
	missing   int
	created   time.Time
}

func inventAlbums(t *testing.T, db *store.Store, now time.Time) {
	t.Helper()

	year := func(y, m, d int) time.Time {
		return time.Date(y, time.Month(m), d, 12, 0, 0, 0, time.Local)
	}
	albums := []invented{
		{firstAlbumID, "Ada's first year", store.AlbumOwned, "", true, 1_412, store.SyncAll, 1_390, 8, 2, 0, year(2023, 4, 2)},
		{"album-iceland", "Iceland, the long way round", store.AlbumOwned, "", true, 968, store.SyncAll, 214, 26, 0, 0, year(2022, 8, 19)},
		{"album-attic", "Scanned from the attic", store.AlbumOwned, "", true, 1_204, store.SyncPicked, 173, 9, 0, 3, year(2021, 1, 30)},
		{"album-kitchen", "The kitchen, before and after", store.AlbumOwned, "", true, 214, store.SyncPicked, 96, 0, 0, 0, year(2024, 11, 8)},
		{"album-sixtieth", "Dad's 60th", store.AlbumShared, "Rita", false, 342, store.SyncAll, 342, 0, 0, 0, year(2019, 6, 15)},
		{"album-walks", "Weekend walks", store.AlbumOwned, "", true, 3_781, store.SyncNone, 0, 0, 0, 0, year(2018, 3, 4)},
		{"album-wedding", "Sam and Jo", store.AlbumBundle, "Sam", false, 512, store.SyncNone, 0, 0, 0, 0, year(2017, 9, 23)},
	}

	for _, album := range albums {
		record := store.Album{
			ID: album.id, Title: album.title, ItemCount: album.items, Kind: album.kind,
			CreatedAt: album.created, CoverURL: "cover:" + album.id,
			OwnerName: album.owner, OwnerIsAccount: album.mine,
		}
		if err := db.UpsertAlbum(record, now.Add(-90*24*time.Hour)); err != nil {
			t.Fatalf("inventing the album %s: %v", album.title, err)
		}
		if err := db.SetAlbumSyncMode(album.id, album.mode); err != nil {
			t.Fatalf("following the album %s: %v", album.title, err)
		}
		if album.mode != store.SyncNone {
			if err := db.MarkAlbumSynced(album.id, now.Add(-19*time.Hour)); err != nil {
				t.Fatalf("dating the album %s: %v", album.title, err)
			}
		}
		inventItems(t, db, album, now)
	}

	// A picture of the favourites filter with nothing starred would not show what the star is for,
	// and starring the album that already sorts first would not show that a star moves one.
	if err := db.SetAlbumFavourite("album-kitchen", true); err != nil {
		t.Fatalf("starring an album: %v", err)
	}

	if err := db.SetLibrary(store.SyncAll, year(2015, 1, 1)); err != nil {
		t.Fatalf("following the whole library: %v", err)
	}
}

// inventItems writes one row per state the album is meant to show. The library is far bigger than
// what is written here: item_count is what Google says the album holds, and a real store is only
// ever partway through catching up with it. The album that gets photographed is held to one
// screenful, because every row it has is a thumbnail in the picture.
func inventItems(t *testing.T, db *store.Store, album invented, now time.Time) {
	t.Helper()

	written := 0
	write := func(index int, state store.State) {
		key := fmt.Sprintf("%s-%04d", album.id, index)
		item := store.MediaItem{
			MediaKey:     key,
			Filename:     key,
			CapturedAt:   inventedCapture(album, key),
			ThumbnailURL: "thumb:" + key,
			IsVideo:      scatter(key+"/video", 11) == 7,
		}
		if err := db.UpsertItem(item, now.Add(-40*24*time.Hour)); err != nil {
			t.Fatalf("inventing an item in %s: %v", album.title, err)
		}
		if err := db.LinkItemToAlbum(album.id, key, now.Add(-40*24*time.Hour)); err != nil {
			t.Fatalf("filing an item under %s: %v", album.title, err)
		}

		switch state {
		case store.StateDone:
			finished := item
			finished.Filename = inventedFilename(key, item.IsVideo)
			finished.SizeBytes = inventedSize(key, item.IsVideo)
			finished.LocalPath = filepath.Join("pool", finished.CapturedAt.Format("2006/01"), finished.Filename)
			finished.MimeType = inventedMimeType(item.IsVideo)
			if err := db.MarkDownloaded(finished, now.Add(-time.Duration(index)*13*time.Minute)); err != nil {
				t.Fatalf("marking an item downloaded in %s: %v", album.title, err)
			}
			// Read for its place, as the sweep would have by now; six in ten carry one, which
			// is what the real library measured.
			located := store.Location{MediaKey: key, Known: scatter(key+"/place", 10) < 6,
				Latitude: 28.4 + float64(scatter(key+"/lat", 40))/100, Longitude: -14.0 - float64(scatter(key+"/lon", 60))/100}
			if err := db.MarkLocated([]store.Location{located}, now); err != nil {
				t.Fatalf("locating an item in %s: %v", album.title, err)
			}
		case store.StateFailed:
			if err := db.MarkFailed(key, fmt.Errorf("the content host answered 403 three times")); err != nil {
				t.Fatalf("failing an item in %s: %v", album.title, err)
			}
		case store.StateMissingUpstream:
			if err := db.MarkMissingUpstream(key, now.Add(-6*24*time.Hour)); err != nil {
				t.Fatalf("losing an item from %s: %v", album.title, err)
			}
		}
		written++
	}

	for index := range album.rowsWorthWriting() {
		write(index, store.StateDone)
	}
	for index := 0; index < album.pending; index++ {
		write(written, store.StateDiscovered)
	}
	for index := 0; index < album.failed; index++ {
		write(written, store.StateFailed)
	}
	for index := 0; index < album.missing; index++ {
		write(written, store.StateMissingUpstream)
	}

	if album.mode == store.SyncPicked {
		if err := db.SelectWholeAlbum(album.id, true); err != nil {
			t.Fatalf("picking the album %s: %v", album.title, err)
		}
	}
}

// rowsWorthWriting keeps the album in the album picture down to one screenful of thumbnails. The
// others are only ever read as numbers, so they are written out in full and the counts on the
// list are the counts the store really holds.
func (i invented) rowsWorthWriting() int {
	const mostAGridShows = 48
	if i.id == firstAlbumID {
		return min(i.done, mostAGridShows)
	}
	return i.done
}

// scatter is the one dice roll in here: a number fixed by the key it is drawn for, so the same
// invented library comes out of every run, and so that dates, sizes and file types vary the way
// a camera's do rather than marching in step with the loop that wrote them.
func scatter(key string, span int) int {
	digest := fnv.New32a()
	digest.Write([]byte(key))
	return int(digest.Sum32() % uint32(span))
}

func inventedCapture(album invented, key string) time.Time {
	return album.created.
		Add(time.Duration(scatter(key+"/day", 300)) * 24 * time.Hour).
		Add(time.Duration(scatter(key+"/hour", 14*60)) * time.Minute)
}

func inventedFilename(key string, video bool) string {
	number := scatter(key+"/number", 9000)
	if video {
		return fmt.Sprintf("MVI_%04d.MOV", number)
	}
	switch scatter(key+"/camera", 3) {
	case 0:
		return fmt.Sprintf("IMG_%04d.HEIC", number)
	case 1:
		return fmt.Sprintf("PXL_2024%02d%02d_%06d.jpg", 1+scatter(key+"/month", 12), 1+scatter(key+"/date", 27), number*13)
	default:
		return fmt.Sprintf("DSC_%04d.NEF", number)
	}
}

func inventedSize(key string, video bool) int64 {
	if video {
		return int64(38_000_000 + scatter(key+"/size", 560)*1_000_000)
	}
	return int64(1_700_000 + scatter(key+"/size", 5_400)*1_000)
}

func inventedMimeType(video bool) string {
	if video {
		return "video/quicktime"
	}
	return "image/heic"
}

func inventRuns(t *testing.T, db *store.Store, now time.Time) {
	t.Helper()

	// Oldest first, because a run's id is the order it was started in and the history is read
	// back by id. Written the other way round, the newest run answers to the lowest number and
	// every page that says "the last run" names the first one.
	runs := []struct {
		startedAgo, ran      time.Duration
		outcome              store.Outcome
		listed, down, failed int
		bytes                int64
		problem              string
	}{
		{115 * time.Hour, 4*time.Hour + 55*time.Minute, store.OutcomeOK, 2_733, 2_733, 0, 88_100_000_000, ""},
		{91 * time.Hour, 7*time.Hour + 3*time.Minute, store.OutcomePartial, 5_881, 5_402, 12, 201_000_000_000,
			"12 items were still failing when the run ran out of work it could do"},
		{67 * time.Hour, 2*time.Hour + 9*time.Minute, store.OutcomeInterrupted, 1_204, 902, 0, 31_800_000_000, ""},
		{43 * time.Hour, 5*time.Hour + 41*time.Minute, store.OutcomeOK, 3_004, 2_961, 3, 96_400_000_000, ""},
		{19 * time.Hour, 6*time.Hour + 12*time.Minute, store.OutcomeOK, 4_112, 3_908, 0, 148_000_000_000, ""},
	}

	for _, run := range runs {
		startedAt := now.Add(-run.startedAgo)
		id, err := db.StartRun(startedAt)
		if err != nil {
			t.Fatalf("inventing a run: %v", err)
		}
		finished := store.SyncRun{
			ID: id, Outcome: run.outcome, Listed: run.listed, Downloaded: run.down,
			Failed: run.failed, Bytes: run.bytes, Error: run.problem,
		}
		if err := db.FinishRun(finished, startedAt.Add(run.ran)); err != nil {
			t.Fatalf("finishing an invented run: %v", err)
		}
	}
}

// paintedThumbnails stands in for Google's thumbnail service. It paints soft blocks of colour
// that read as photographs at grid size without being anybody's, and paints the same one for
// the same key every time so the pictures do not churn between runs.
type paintedThumbnails struct{}

func (paintedThumbnails) Thumbnail(_ context.Context, baseURL string, w io.Writer) (int64, error) {
	seed := fnv.New64a()
	seed.Write([]byte(baseURL))
	pattern := seed.Sum64()

	width, height := 800, 600
	if pattern%3 == 0 {
		width, height = 600, 800
	}

	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	blobs := blobsFrom(pattern, width, height)
	for y := range height {
		for x := range width {
			canvas.Set(x, y, blend(blobs, x, y))
		}
	}

	counter := &countingWriter{to: w}
	if err := jpeg.Encode(counter, canvas, &jpeg.Options{Quality: 82}); err != nil {
		return 0, err
	}
	return counter.written, nil
}

type blob struct {
	x, y, radius float64
	shade        color.RGBA
}

func blobsFrom(pattern uint64, width, height int) []blob {
	hues := []color.RGBA{
		{0x2f, 0x4f, 0x6a, 0xff}, {0x7a, 0x9e, 0x7e, 0xff}, {0xd8, 0xa6, 0x57, 0xff},
		{0xa8, 0x5c, 0x4e, 0xff}, {0x4c, 0x62, 0x8a, 0xff}, {0xc9, 0xb4, 0x9a, 0xff},
		{0x35, 0x3d, 0x4a, 0xff}, {0xe0, 0xc8, 0xa8, 0xff},
	}

	blobs := make([]blob, 0, 5)
	for at := range 5 {
		spin := pattern >> (at * 9)
		blobs = append(blobs, blob{
			x:      float64(width) * float64(spin%97) / 96,
			y:      float64(height) * float64((spin/97)%89) / 88,
			radius: float64(width) * (0.35 + float64((spin/8191)%40)/100),
			shade:  hues[(spin/13)%uint64(len(hues))],
		})
	}
	return blobs
}

// blend is a crude painter's light: every blob contributes in proportion to how near it is, so
// the edges are soft and the result looks like a photograph too far away to make out.
func blend(blobs []blob, x, y int) color.RGBA {
	var red, green, blue, weight float64
	for _, near := range blobs {
		distance := math.Hypot(float64(x)-near.x, float64(y)-near.y)
		strength := math.Max(0, 1-distance/near.radius)
		strength *= strength
		red += float64(near.shade.R) * strength
		green += float64(near.shade.G) * strength
		blue += float64(near.shade.B) * strength
		weight += strength
	}
	if weight == 0 {
		return color.RGBA{0x20, 0x24, 0x2c, 0xff}
	}
	return color.RGBA{uint8(red / weight), uint8(green / weight), uint8(blue / weight), 0xff}
}

type countingWriter struct {
	to      io.Writer
	written int64
}

func (c *countingWriter) Write(payload []byte) (int, error) {
	n, err := c.to.Write(payload)
	c.written += int64(n)
	return n, err
}
