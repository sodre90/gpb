package gphotos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// thumbnailSizeSuffix fits the image inside a 256-pixel box, preserving aspect ratio.
// Measured 2026-08-10: the bare base URL returns a 90 KB image at some default size, this
// suffix returns 24 KB, and the "-c" cropping variant returns a larger square. The grid
// crops in CSS, so the uncropped form is both smaller and more useful.
const thumbnailSizeSuffix = "=w256-h256"

// maxThumbnailSize bounds what a single grid cell may cost us. A 256-pixel JPEG is tens of
// kilobytes; anything approaching this limit means the URL is not serving what we think.
const maxThumbnailSize = 4 << 20

// ErrNoThumbnail means the item carries no thumbnail URL to fetch. The grid renders a
// placeholder rather than treating it as a failure.
var ErrNoThumbnail = errors.New("the item has no thumbnail URL")

// thumbnailHosts is the allowlist of hosts allowed to receive the account's cookies for a
// thumbnail. Measured across 1586 items in 10 albums on 2026-08-10, every thumbnail URL was
// on this one host, and every one of them 403s without the session — so the cookies are
// mandatory and the host must be pinned. An unfamiliar host is refused rather than trusted.
var thumbnailHosts = map[string]bool{
	"photos.fife.usercontent.google.com": true,
}

// Thumbnail streams one grid-sized image into w. The base URL comes from a listing and is an
// opaque content id — no signature, no expiry — so it is safe for the store to keep.
func (c *Client) Thumbnail(ctx context.Context, baseURL string, w io.Writer) (int64, error) {
	if baseURL == "" {
		return 0, ErrNoThumbnail
	}
	if err := c.checkThumbnailHost(baseURL); err != nil {
		return 0, err
	}
	if err := c.wait(ctx); err != nil {
		return 0, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+thumbnailSizeSuffix, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("User-Agent", c.userAgent)

	response, err := c.downloads.Do(request)
	if err != nil {
		return 0, fmt.Errorf("fetching a thumbnail: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return 0, thumbnailStatusError(response)
	}

	written, err := io.Copy(w, io.LimitReader(response.Body, maxThumbnailSize))
	if err != nil {
		return written, fmt.Errorf("streaming a thumbnail: %w", err)
	}
	return written, nil
}

// checkThumbnailHost is the same discipline the download path applies: never decide at
// runtime that an unfamiliar host deserves the session.
func (c *Client) checkThumbnailHost(baseURL string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("%w: the thumbnail URL does not parse", ErrUnknownContentHost)
	}
	if !thumbnailHosts[parsed.Host] {
		return fmt.Errorf("%w: %s", ErrUnknownContentHost, parsed.Host)
	}
	if !strings.HasPrefix(baseURL, "https://") {
		return fmt.Errorf("%w: the thumbnail URL is not https", ErrUnknownContentHost)
	}
	return nil
}

// thumbnailStatusError distinguishes a dead session from a dead URL. A 403 here means the
// cookies stopped working — the same URL served fine an hour ago — so it is worth reporting
// as a session problem rather than an image problem.
func thumbnailStatusError(response *http.Response) error {
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: HTTP %d for a thumbnail", ErrSessionRejected, response.StatusCode)
	default:
		return &HTTPError{StatusCode: response.StatusCode, RetryAfter: retryAfter(response)}
	}
}
