package gphotos

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadFixture(t *testing.T, name string) any {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the %s fixture: %v", name, err)
	}

	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("the %s fixture is not JSON: %v", name, err)
	}
	return payload
}

func TestDecodeAlbumPage(t *testing.T) {
	page, err := decodeAlbumPage(loadFixture(t, "albums.json"))
	if err != nil {
		t.Fatalf("decoding the album fixture: %v", err)
	}

	if len(page.Albums) != 5 {
		t.Fatalf("decoded %d albums, want 5", len(page.Albums))
	}
	if page.NextToken == "" {
		t.Error("the album page carried no continuation token")
	}

	first := page.Albums[0]
	if first.Title != "Test Album 1" {
		t.Errorf("first album title is %q", first.Title)
	}
	if first.ItemCount != 1150 {
		t.Errorf("first album item count is %d, want 1150", first.ItemCount)
	}
	if want := time.UnixMilli(1767270992307).UTC(); !first.CreatedAt.Equal(want) {
		t.Errorf("first album created at %s, want %s", first.CreatedAt, want)
	}
	if !strings.HasPrefix(first.CoverURL, "https://") {
		t.Errorf("first album cover URL is %q", first.CoverURL)
	}
}

// Untitled albums are real: 3 of the first 25 in the Phase 0 capture had a null title. A
// decoder that treated the title as required would reject a perfectly valid library.
func TestUntitledAlbumsSurviveDecoding(t *testing.T) {
	page, err := decodeAlbumPage(loadFixture(t, "albums.json"))
	if err != nil {
		t.Fatalf("decoding the album fixture: %v", err)
	}

	untitled := 0
	for _, album := range page.Albums {
		if album.Title == "" {
			untitled++
		}
		if album.ID == "" {
			t.Fatal("an album decoded without an id")
		}
	}
	if untitled != 2 {
		t.Errorf("decoded %d untitled albums, want 2", untitled)
	}
}

// Measured live on 2026-08-10: the entries the album listing marks with a 4 are not albums.
// Their own page is headed "Shared photos", their contents response carries no title, and they
// appear nowhere in the albums UI. Without this distinction the list calls 49 of a 181-entry
// library "untitled" as though the user had forgotten to name them.
func TestBundlesOfSharedPhotosAreNotCalledAlbums(t *testing.T) {
	page, err := decodeAlbumPage(loadFixture(t, "albums.json"))
	if err != nil {
		t.Fatalf("decoding the album fixture: %v", err)
	}

	bundles, bundleWithATitle, ownedWithout := 0, 0, 0
	for _, album := range page.Albums {
		switch {
		case album.Kind == AlbumBundle && album.Title != "":
			bundleWithATitle++
			bundles++
		case album.Kind == AlbumBundle:
			bundles++
		case album.Title == "":
			ownedWithout++
		}
	}

	if bundles != 2 {
		t.Errorf("decoded %d bundles, want 2", bundles)
	}
	if bundleWithATitle != 0 || ownedWithout != 0 {
		t.Errorf("the fixture no longer matches what was measured: %d bundles carry a title, "+
			"%d owned albums carry none", bundleWithATitle, ownedWithout)
	}
}

// Google names a bundle of shared photos nowhere, so whoever shared it is the only thing that
// tells one from the next. Measured 2026-08-10 the owner decoded on all 181 entries.
func TestAlbumsSayWhoTheyCameFrom(t *testing.T) {
	page, err := decodeAlbumPage(loadFixture(t, "albums.json"))
	if err != nil {
		t.Fatalf("decoding the album fixture: %v", err)
	}

	for _, album := range page.Albums {
		if album.Owner.ID == "" || album.Owner.Name == "" {
			t.Errorf("album %q decoded without an owner: %+v", album.Title, album.Owner)
		}
	}

	// The listing carries no flag for "this one is yours" — the owner's id is what says so, and
	// a fixture where every entry names the same person could not prove the decoder reads it.
	owners := map[string]bool{}
	for _, album := range page.Albums {
		owners[album.Owner.ID] = true
	}
	if len(owners) < 2 {
		t.Error("every entry decoded to the same owner, so this proves nothing")
	}
}

// The album listing is not the library. Measured live on 2026-08-10, this second listing
// returned 18 albums shared with the account — 3714 photos — that the album listing omits
// entirely, including the one the user reported as wrongly "untitled".
func TestSharedAlbumListingCarriesTitles(t *testing.T) {
	page, err := decodeSharedAlbumPage(loadFixture(t, "shared-albums.json"))
	if err != nil {
		t.Fatalf("decoding the shared album fixture: %v", err)
	}

	if len(page.Albums) != 3 {
		t.Fatalf("decoded %d shared albums, want 3", len(page.Albums))
	}
	if page.NextToken == "" {
		t.Error("the shared album page carried no continuation token")
	}

	first := page.Albums[0]
	if first.Title != "Album Shared With Me" {
		t.Errorf("first shared album title is %q", first.Title)
	}
	if first.ItemCount != 3 {
		t.Errorf("first shared album holds %d items, want 3", first.ItemCount)
	}
	if want := time.UnixMilli(1631443404613).UTC(); !first.CreatedAt.Equal(want) {
		t.Errorf("first shared album created at %s, want %s", first.CreatedAt, want)
	}
	if !strings.HasPrefix(first.CoverURL, "https://") {
		t.Errorf("first shared album cover URL is %q", first.CoverURL)
	}
}

// The record sits at the end of the entry, and the entry runs to 12 slots for an album this
// account owns against 9 for one shared with it. A decoder reading a fixed index would find
// the title of one and null for the other.
func TestSharedAlbumsDecodeAtEitherEntryLength(t *testing.T) {
	page, err := decodeSharedAlbumPage(loadFixture(t, "shared-albums.json"))
	if err != nil {
		t.Fatalf("decoding the shared album fixture: %v", err)
	}

	if owned := page.Albums[1]; owned.Title != "An Album I Own" || owned.ItemCount != 42 {
		t.Errorf("the longer entry decoded as %q with %d items", owned.Title, owned.ItemCount)
	}
	if untitled := page.Albums[2]; untitled.Title != "" || untitled.ItemCount != 7 {
		t.Errorf("an untitled shared album decoded as %q with %d items",
			untitled.Title, untitled.ItemCount)
	}
}

// Which of the two a shared-listing entry is cannot be read out of the entry, so the decoder
// must not guess: the caller decides from whether the album listing already claimed the id.
func TestTheSharedListingDoesNotClaimAKind(t *testing.T) {
	page, err := decodeSharedAlbumPage(loadFixture(t, "shared-albums.json"))
	if err != nil {
		t.Fatalf("decoding the shared album fixture: %v", err)
	}

	for _, album := range page.Albums {
		if album.Kind != "" {
			t.Errorf("the shared listing decoded a kind of %q", album.Kind)
		}
	}
}

func TestDecodeItemPage(t *testing.T) {
	page, err := decodeItemPage(loadFixture(t, "album-items.json"))
	if err != nil {
		t.Fatalf("decoding the item fixture: %v", err)
	}

	if len(page.Items) != 6 {
		t.Fatalf("decoded %d items, want 6", len(page.Items))
	}
	if page.NextToken != "" {
		t.Errorf("the fixture is the last page but reported the token %q", page.NextToken)
	}
	if page.AlbumTitle != "Test Album 2" {
		t.Errorf("album title decoded as %q", page.AlbumTitle)
	}

	first := page.Items[0]
	if first.Width != 4000 || first.Height != 3000 {
		t.Errorf("first item is %dx%d, want 4000x3000", first.Width, first.Height)
	}
	if want := time.UnixMilli(1785664382360).UTC(); !first.CapturedAt.Equal(want) {
		t.Errorf("first item captured at %s, want %s", first.CapturedAt, want)
	}
	if first.TimezoneOffset != 2*time.Hour {
		t.Errorf("first item timezone offset is %s, want 2h", first.TimezoneOffset)
	}
}

func TestVideosAreDistinguishedFromStills(t *testing.T) {
	page, err := decodeItemPage(loadFixture(t, "album-items.json"))
	if err != nil {
		t.Fatalf("decoding the item fixture: %v", err)
	}

	videos := 0
	for _, item := range page.Items {
		if !item.IsVideo {
			if item.Duration != 0 {
				t.Errorf("still %s carries a duration", item.MediaKey)
			}
			continue
		}
		videos++
		if item.Duration <= 0 {
			t.Errorf("video %s decoded without a duration", item.MediaKey)
		}
	}
	if videos != 2 {
		t.Errorf("found %d videos, want 2", videos)
	}
}

// The library listing keeps its entries and cursor in different slots from an album's, and
// nothing but position tells them apart — so the two pages need separate proof.
func TestDecodeTimelinePage(t *testing.T) {
	page, err := decodeTimelinePage(loadFixture(t, "timeline.json"))
	if err != nil {
		t.Fatalf("decoding the timeline fixture: %v", err)
	}

	if len(page.Items) != 8 {
		t.Fatalf("decoded %d items, want 8", len(page.Items))
	}
	if page.NextToken == "" {
		t.Error("the fixture is a first page of 122,331 but reported no continuation token")
	}

	newest := page.Items[0]
	if want := time.UnixMilli(1785662569974).UTC(); !newest.CapturedAt.Equal(want) {
		t.Errorf("first item captured at %s, want %s", newest.CapturedAt, want)
	}
	if newest.ThumbnailURL == "" {
		t.Error("first item decoded without a thumbnail")
	}

	videos := 0
	for _, item := range page.Items {
		if item.IsVideo {
			videos++
			if item.Duration <= 0 {
				t.Errorf("video %s decoded without a duration", item.MediaKey)
			}
		}
	}
	if videos != 2 {
		t.Errorf("found %d videos, want 2", videos)
	}
}

// The pool buckets by capture date, so an evening photo must not slide into the next day
// just because UTC did.
func TestLocalCaptureTimeUsesTheCameraTimezone(t *testing.T) {
	item := MediaItem{
		CapturedAt:     time.Date(2026, 8, 2, 22, 30, 0, 0, time.UTC),
		TimezoneOffset: 2 * time.Hour,
	}

	local := item.LocalCaptureTime()
	if local.Day() != 3 || local.Hour() != 0 {
		t.Errorf("local capture time is %s, want 2026-08-03 00:30 local", local)
	}
}

func TestDecodeDownloadURL(t *testing.T) {
	root := rootOf(loadFixture(t, "download-url.json"), downloadURLRPC)

	signed, ok := root.at(downloadURLIndex).text()
	if !ok || !strings.HasPrefix(signed, "https://") {
		t.Fatalf("the signed URL decoded as %q", signed)
	}
}

// The session is the account's credentials. It goes to the one content host that refuses the
// download without it, and nowhere else — least of all a host chosen by a remote response.
func TestOnlyKnownContentHostsAreFetched(t *testing.T) {
	client := &Client{downloads: &http.Client{}}

	cases := map[string]struct {
		wantSession bool
		wantKnown   bool
	}{
		"https://video-downloads.googleusercontent.com/ADGPM2k":        {false, true},
		"https://photos.fife.usercontent.google.com/pw/AP1Gcz":         {true, true},
		"https://lh3.googleusercontent.com/pw/AP1GczP=d":               {false, false},
		"https://lh3.google.com/pwu/AAYg69Zj":                          {false, false},
		"https://video-downloads.googleusercontent.com.evil.example/x": {false, false},
		"https://photos.fife.usercontent.google.com.evil.example/x":    {false, false},
		"https://evil.example/pw/AP1Gcz":                               {false, false},
	}

	for candidate, want := range cases {
		transport, err := client.transportFor(candidate)
		if !want.wantKnown {
			if !errors.Is(err, ErrUnknownContentHost) {
				t.Errorf("%q was accepted, want a refusal", candidate)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q was refused: %v", candidate, err)
			continue
		}
		if carriedSession := transport != anonymousTransport; carriedSession != want.wantSession {
			t.Errorf("%q carried the session = %v, want %v", candidate, carriedSession, want.wantSession)
		}
	}
}

// cannedResponse answers every request with one prepared reply, which is how the download path
// gets exercised without a network: the only hosts this client will fetch from are Google's, so
// a local test server would be refused before it was ever asked.
type cannedResponse struct{ response *http.Response }

func (c cannedResponse) RoundTrip(*http.Request) (*http.Response, error) { return c.response, nil }

// listeningWriter is the optional upgrade Fetch looks for. What it is told is the only source
// either fact has: the listing carries no filename, and nothing knows how big an item is until
// it has been downloaded.
type listeningWriter struct {
	name  string
	total int64
}

func (w *listeningWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *listeningWriter) Announce(filename string, total int64) {
	w.name, w.total = filename, total
}

func TestFetchTellsTheWriterWhatTheFileIsBeforeTheBytesArrive(t *testing.T) {
	const signedURL = "https://photos.fife.usercontent.google.com/pw/AP1Gcz"

	cases := map[string]struct {
		offset    int64
		status    int
		length    int64
		wantName  string
		wantTotal int64
	}{
		"whole file":                {offset: 0, status: http.StatusOK, length: 40, wantName: "IMG_2044.MOV", wantTotal: 40},
		"resumed after a first try": {offset: 10, status: http.StatusPartialContent, length: 40, wantName: "IMG_2044.MOV", wantTotal: 50},
		// A host that will not say how long the body is leaves no denominator, and a zero says so
		// — the name is still worth having.
		"length withheld": {offset: 0, status: http.StatusOK, length: -1, wantName: "IMG_2044.MOV", wantTotal: 0},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			body := strings.Repeat("x", 40)
			header := http.Header{"Content-Disposition": []string{`attachment; filename="IMG_2044.MOV"`}}
			client := &Client{downloads: &http.Client{Transport: cannedResponse{&http.Response{
				StatusCode:    testCase.status,
				Header:        header,
				Body:          io.NopCloser(strings.NewReader(body)),
				ContentLength: testCase.length,
			}}}}

			writer := &listeningWriter{}
			if _, err := client.Fetch(t.Context(), signedURL, testCase.offset, writer); err != nil {
				t.Fatalf("fetching: %v", err)
			}
			if writer.name != testCase.wantName {
				t.Errorf("the writer was told the file is called %q, want %q", writer.name, testCase.wantName)
			}
			if writer.total != testCase.wantTotal {
				t.Errorf("the writer was told the file is %d bytes, want %d", writer.total, testCase.wantTotal)
			}
		})
	}
}

func TestDecodersRejectDriftWithAPath(t *testing.T) {
	cases := map[string]struct {
		payload any
		decode  func(any) error
		want    string
	}{
		"albums is not an array": {
			payload: []any{"not an array", "token"},
			decode:  func(p any) error { _, err := decodeAlbumPage(p); return err },
			want:    "F2A0H[0]",
		},
		"an album lost its id": {
			payload: []any{[]any{[]any{1.0, "title"}}, "token"},
			decode:  func(p any) error { _, err := decodeAlbumPage(p); return err },
			want:    "F2A0H[0][0][6]",
		},
		"items are not an array": {
			payload: []any{nil, "not an array"},
			decode:  func(p any) error { _, err := decodeItemPage(p); return err },
			want:    "snAcKc[1]",
		},
		"an item lost its media key": {
			payload: []any{nil, []any{[]any{nil, nil}}},
			decode:  func(p any) error { _, err := decodeItemPage(p); return err },
			want:    "snAcKc[1][0][0]",
		},
		"an album page cursor is not a string": {
			payload: []any{[]any{}, 42.0},
			decode:  func(p any) error { _, err := decodeAlbumPage(p); return err },
			want:    "F2A0H[1]",
		},
		"a shared album page cursor is not a string": {
			payload: []any{[]any{}, 42.0},
			decode:  func(p any) error { _, err := decodeSharedAlbumPage(p); return err },
			want:    "Z5xsfc[1]",
		},
		"an item page cursor is not a string": {
			payload: []any{nil, []any{}, 42.0},
			decode:  func(p any) error { _, err := decodeItemPage(p); return err },
			want:    "snAcKc[2]",
		},
		"a timeline cursor is not a string": {
			payload: []any{[]any{}, 42.0},
			decode:  func(p any) error { _, err := decodeTimelinePage(p); return err },
			want:    "lcxiM[1]",
		},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			err := test.decode(test.payload)
			if !errors.Is(err, ErrProtocolDrift) {
				t.Fatalf("got %v, want an ErrProtocolDrift", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("the drift error does not name %s: %v", test.want, err)
			}
		})
	}
}

// A missing items array is reported the same way whether the album is empty, gone, or genuinely
// drifted, so the position on its own does not say which happened. The payload skeleton is what
// separates them, and it is the only thing in the error a maintainer can act on.
func TestDriftErrorsCarryThePayloadSkeleton(t *testing.T) {
	cases := map[string]struct {
		payload any
		want    string
	}{
		"no payload at all":       {payload: nil, want: "the payload is null"},
		"an empty envelope":       {payload: []any{}, want: "the payload is []"},
		"an envelope with a hole": {payload: []any{nil}, want: "the payload is [null]"},
		"a page that kept its cursor": {
			payload: []any{nil, nil, "cursor"},
			want:    "the payload is [null,null,str]",
		},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decodeItemPage(test.payload)
			if !errors.Is(err, ErrProtocolDrift) {
				t.Fatalf("got %v, want an ErrProtocolDrift", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("the drift error does not describe the payload as %q: %v", test.want, err)
			}
		})
	}
}

// The skeleton travels in logs and notifications, so it may report arity and type and nothing
// else. A media key, a signed URL or an album title in a drift report is a leak.
func TestThePayloadSkeletonQuotesNoValues(t *testing.T) {
	payload := []any{
		"AF1Qip371E574DEE0F2D643E2D8F1A357BD630844BC6",
		map[string]any{"76647426": []any{4000.0}},
		"https://photos.fife.usercontent.google.com/scrubbed/7ecf1b4e",
	}

	err := rootOf(payload, albumItemsRPC).at(9).driftf("something absent")
	for _, secret := range []string{"AF1Qip", "76647426", "usercontent", "4000"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the drift error leaks %q: %v", secret, err)
		}
	}
}

// The empty page token is how every caller learns a walk finished, and the syncer reconciles
// an album against that — anything it did not see this pass is recorded as gone from Google.
// So the last page must still decode, and only a genuinely absent cursor may produce the
// token that authorises the deletion.
func TestTheLastPageEndsAWalkAndNothingElseDoes(t *testing.T) {
	decoders := map[string]struct {
		lastPage any
		decode   func(any) (string, error)
	}{
		"albums": {
			lastPage: []any{[]any{}, nil},
			decode:   func(p any) (string, error) { page, err := decodeAlbumPage(p); return page.NextToken, err },
		},
		"shared albums": {
			lastPage: []any{[]any{}, nil},
			decode:   func(p any) (string, error) { page, err := decodeSharedAlbumPage(p); return page.NextToken, err },
		},
		"album items": {
			lastPage: []any{nil, []any{}, nil},
			decode:   func(p any) (string, error) { page, err := decodeItemPage(p); return page.NextToken, err },
		},
		"timeline": {
			lastPage: []any{[]any{}, nil},
			decode:   func(p any) (string, error) { page, err := decodeTimelinePage(p); return page.NextToken, err },
		},
	}

	for name, decoder := range decoders {
		t.Run(name, func(t *testing.T) {
			token, err := decoder.decode(decoder.lastPage)
			if err != nil {
				t.Fatalf("the last page of the %s listing did not decode: %v", name, err)
			}
			if token != "" {
				t.Errorf("the last page of the %s listing carried a token %q", name, token)
			}
		})
	}
}

// A drift report travels into logs and notifications, so it must describe the shape it
// found without echoing media keys, album titles or signed URLs.
func TestDriftErrorsDoNotLeakPayloadContents(t *testing.T) {
	secret := "AF1QipSECRETmediakey"
	_, err := decodeAlbumPage([]any{[]any{[]any{1.0, secret, nil, nil, nil, nil, 42.0}}, "token"})

	if err == nil {
		t.Fatal("a numeric album id was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the drift error leaked payload contents: %v", err)
	}
}

func TestDecodeFramesReadsChunkedResponses(t *testing.T) {
	body := ")]}'\n\n38\n[[\"wrb.fr\",\"F2A0H\",\"[[],null]\",null,null,null,\"generic\"]]\n"

	frames, err := decodeFrames(body)
	if err != nil {
		t.Fatalf("decoding a chunked response: %v", err)
	}

	payload, err := payloadFor(frames, "F2A0H")
	if err != nil {
		t.Fatalf("selecting the F2A0H frame: %v", err)
	}
	if _, ok := payload.([]any); !ok {
		t.Fatalf("the payload decoded as %T, want an array", payload)
	}
}

func TestDecodeFramesRejectsAnUnguardedBody(t *testing.T) {
	if _, err := decodeFrames("wat"); !errors.Is(err, ErrProtocolDrift) {
		t.Fatalf("an unguarded body decoded as %v, want drift", err)
	}
}

// Google answers a dead session with its signed-out page, at 200 and with nothing to redirect
// to. Reported as drift it stops the run saying Google changed shape, which is the one message
// that sends the user looking for a bug rather than at the sign-in button.
func TestAnHTMLAnswerIsReadAsASignedOutSessionRatherThanDrift(t *testing.T) {
	for _, body := range []string{
		"<!doctype html><html>sign in</html>",
		"<!DOCTYPE HTML>\n<html lang=\"en\">",
		"\n  <html><body>Sign in - Google Accounts</body></html>",
	} {
		_, err := decodeFrames(body)
		if !errors.Is(err, ErrSessionRejected) {
			t.Errorf("a page answer decoded as %v, want a rejected session", err)
		}
		if errors.Is(err, ErrProtocolDrift) {
			t.Errorf("a page answer was also reported as drift: %v", err)
		}
	}
}

func TestPayloadForNamesTheFramesItSaw(t *testing.T) {
	frames := []frame{{tag: "wrb.fr", rpcID: "snAcKc", payload: "[]"}}

	_, err := payloadFor(frames, "F2A0H")
	if !errors.Is(err, ErrProtocolDrift) {
		t.Fatalf("got %v, want drift", err)
	}
	if !strings.Contains(err.Error(), "snAcKc") {
		t.Errorf("the error does not say which frames arrived: %v", err)
	}
}

// Content-Disposition is remote input that becomes a filesystem path.
func TestSanitizeFilenameStripsPathTraversal(t *testing.T) {
	cases := map[string]string{
		"IMG_2041.HEIC":             "IMG_2041.HEIC",
		"../../../etc/passwd":       "passwd",
		"/absolute/path/photo.jpg":  "photo.jpg",
		`..\..\windows\system32.db`: "system32.db",
		"":                          "",
		"..":                        "",
		"/":                         "",
	}

	for input, want := range cases {
		if got := sanitizeFilename(input); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHTTPErrorRetryPolicy(t *testing.T) {
	cases := map[int]bool{
		429: true, 500: true, 503: true,
		400: false, 404: false, 418: false,
	}

	for status, want := range cases {
		if got := (&HTTPError{StatusCode: status}).Retryable(); got != want {
			t.Errorf("HTTP %d retryable = %v, want %v", status, got, want)
		}
	}
}
