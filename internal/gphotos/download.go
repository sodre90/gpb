package gphotos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const downloadURLRPC = "VrseUb"

// downloadURLIndex is where the minted signed URL sits in the VrseUb answer.
const downloadURLIndex = 1

// downloadTimeout has to cover a half-gigabyte video on a domestic uplink, which the Phase 0
// spike measured at 522 MB for a single clip.
const downloadTimeout = 2 * time.Hour

// ErrRangeIgnored means the server sent the whole file when we asked to resume part of one.
// The caller must restart that file rather than append and corrupt it.
var ErrRangeIgnored = errors.New("the server ignored the requested byte range")

// ErrSignedURLRejected means the content host refused the signed URL. It is deliberately not
// ErrSessionRejected: the usual cause is an expired signature, so the remedy is to mint a
// fresh URL rather than to re-authenticate.
var ErrSignedURLRejected = errors.New("the content host rejected the signed download URL")

// ErrTruncated means the body ended before the length the server promised. It is retryable:
// the usual cause is a dropped connection, and the partial file can be resumed.
var ErrTruncated = errors.New("the download ended before the advertised length")

// ErrUnknownContentHost means VrseUb minted a URL somewhere this client does not recognise.
// It is fatal rather than best-effort: an unrecognised host is either protocol drift or a
// redirect, and the client must not decide on the fly whether to hand it the session.
var ErrUnknownContentHost = errors.New("google minted a download URL on an unrecognised host")

// contentHosts maps each host VrseUb mints download URLs on to whether it demands the Google
// session on top of the URL's own signature.
//
// Both hosts serve the camera's untouched file — measured 2026-08-10 across 22 items, every
// one arriving at its advertised resolution with an attachment disposition, the fife files
// still carrying the maker note that Google's display re-encoder strips. Which host an item
// lands on is not a quality distinction, and refusing the fife host would silently skip a
// third of a shared album.
var contentHosts = map[string]bool{
	"video-downloads.googleusercontent.com": false,
	"photos.fife.usercontent.google.com":    true,
}

type Download struct {
	Filename    string
	ContentType string
	Size        int64
	Resumed     bool
}

// Announcer is an optional upgrade for the writer Fetch streams into: a writer that implements
// it is told what the file is called and how long it is before any of it arrives. The response
// headers are the only place either fact exists — the listing carries no filename, and nothing
// knows an item's size until it has been fetched — so a writer that wants to say what it is
// downloading, or how far it has got, has nowhere else to get it.
type Announcer interface {
	Announce(filename string, total int64)
}

// DownloadURL mints a signed, short-lived URL for one item. The album id is context Google
// wants for permission checks, and a photo reached through the library timeline has no album
// to name — measured 2026-08-10, a null album id mints a URL and an empty string is refused,
// so the two must not be conflated.
func (c *Client) DownloadURL(ctx context.Context, mediaKey, albumID string) (string, error) {
	payload, err := c.call(ctx, downloadURLRPC,
		[]any{mediaKey, nil, nil, nil, nullable(albumID)}, "/photo/"+mediaKey)
	if err != nil {
		return "", err
	}

	root := rootOf(payload, downloadURLRPC)
	signed, ok := root.at(downloadURLIndex).text()
	if !ok || !strings.HasPrefix(signed, "https://") {
		return "", root.at(downloadURLIndex).driftf("a signed download URL")
	}

	return signed, nil
}

// DownloadOriginal mints a fresh URL and streams the original bytes into w. Minting per
// attempt rather than caching the URL sidesteps the unmeasured signed-URL lifetime: a
// resume hours later gets a new URL instead of a mysterious 403.
func (c *Client) DownloadOriginal(ctx context.Context, mediaKey, albumID string, offset int64, w io.Writer) (Download, error) {
	signed, err := c.DownloadURL(ctx, mediaKey, albumID)
	if err != nil {
		return Download{}, err
	}
	return c.Fetch(ctx, signed, offset, w)
}

// Fetch streams a signed URL into w, optionally resuming from a byte offset.
func (c *Client) Fetch(ctx context.Context, signedURL string, offset int64, w io.Writer) (Download, error) {
	transport, err := c.transportFor(signedURL)
	if err != nil {
		return Download{}, err
	}
	if err := c.wait(ctx); err != nil {
		return Download{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return Download{}, err
	}
	request.Header.Set("User-Agent", c.userAgent)
	if offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	response, err := transport.Do(request)
	if err != nil {
		return Download{}, fmt.Errorf("fetching the original: %w", withoutAddress(err))
	}
	defer response.Body.Close()

	switch {
	case offset > 0 && response.StatusCode == http.StatusOK:
		return Download{}, ErrRangeIgnored
	case offset > 0 && response.StatusCode != http.StatusPartialContent:
		return Download{}, contentStatusError(response)
	case offset == 0 && response.StatusCode != http.StatusOK:
		return Download{}, contentStatusError(response)
	}

	if announcer, ok := w.(Announcer); ok {
		announcer.Announce(filenameFrom(response), announcedLength(offset, response.ContentLength))
	}

	written, err := io.Copy(w, response.Body)
	if err != nil {
		return Download{}, fmt.Errorf("streaming the original after %d bytes: %w", written, err)
	}
	// A connection cut mid-body reads as a clean EOF, so without this a truncated file would
	// be committed to the pool as a complete backup.
	if response.ContentLength >= 0 && written != response.ContentLength {
		return Download{}, fmt.Errorf("%w: the body ended after %d of %d bytes",
			ErrTruncated, written, response.ContentLength)
	}

	return Download{
		Filename:    filenameFrom(response),
		ContentType: response.Header.Get("Content-Type"),
		Size:        offset + written,
		Resumed:     offset > 0,
	}, nil
}

// transportFor withholds the session from any host that does not demand it, and refuses
// outright to fetch from a host that is not in the table — the alternative would be deciding
// at runtime whether an unfamiliar host deserves the account's cookies.
func (c *Client) transportFor(signedURL string) (*http.Client, error) {
	parsed, err := url.Parse(signedURL)
	if err != nil {
		return nil, fmt.Errorf("%w: the URL does not parse", ErrUnknownContentHost)
	}

	needsSession, known := contentHosts[parsed.Host]
	switch {
	case !known:
		return nil, fmt.Errorf("%w: %s", ErrUnknownContentHost, parsed.Host)
	case needsSession:
		return c.downloads, nil
	default:
		return anonymousTransport, nil
	}
}

// anonymousTransport carries no cookie jar by construction, so the host that authenticates on
// the URL signature alone can never be handed the session even by mistake.
var anonymousTransport = &http.Client{}

func contentStatusError(response *http.Response) error {
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusGone:
		return fmt.Errorf("%w: HTTP %d", ErrSignedURLRejected, response.StatusCode)
	default:
		return &HTTPError{StatusCode: response.StatusCode, RetryAfter: retryAfter(response)}
	}
}

// announcedLength is zero when the server did not say how long the body would be, which the
// writer reads as "no percentage is possible" rather than as an empty file.
func announcedLength(offset, contentLength int64) int64 {
	if contentLength < 0 {
		return 0
	}
	return offset + contentLength
}

// filenameFrom reads the name Google intends for the file and strips any directory part.
// The header is remote input that ends up in a filesystem path, so a "../" in it must not
// survive to the pool.
func filenameFrom(response *http.Response) string {
	_, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	return sanitizeFilename(params["filename"])
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(path.Clean("/" + name))
	if name == "/" || name == "." {
		return ""
	}
	return name
}
