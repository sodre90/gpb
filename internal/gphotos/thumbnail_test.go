package gphotos

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A thumbnail fetch carries the account's cookies — every thumbnail URL 403s without them.
// Handing them to whatever host a decoded listing happened to name is exactly the mistake
// the download path already refuses to make.
func TestOnlyKnownThumbnailHostsAreFetched(t *testing.T) {
	client := &Client{downloads: &http.Client{}}

	cases := map[string]bool{
		"https://photos.fife.usercontent.google.com/pw/AP1Gcz":      true,
		"https://photos.fife.usercontent.google.com.evil.example/x": false,
		"https://lh3.googleusercontent.com/pw/AP1Gcz":               false,
		"https://evil.example/pw/AP1Gcz":                            false,
		"http://photos.fife.usercontent.google.com/pw/AP1Gcz":       false,
		"not-a-url-at-all": false,
	}

	for candidate, allowed := range cases {
		err := client.checkThumbnailHost(candidate)
		switch {
		case allowed && err != nil:
			t.Errorf("%q was refused: %v", candidate, err)
		case !allowed && !errors.Is(err, ErrUnknownContentHost):
			t.Errorf("%q gave %v, want a refusal", candidate, err)
		}
	}
}

// An item with no thumbnail is a placeholder in the grid, not a failure worth logging. The
// distinct error is what lets the handler tell the two apart.
func TestThumbnailReportsAMissingURLDistinctly(t *testing.T) {
	client := &Client{downloads: &http.Client{}}

	if _, err := client.Thumbnail(t.Context(), "", &bytes.Buffer{}); !errors.Is(err, ErrNoThumbnail) {
		t.Errorf("an empty URL gave %v, want ErrNoThumbnail", err)
	}
}

// The size suffix is the difference between a 24 KB grid image and a 90 KB one, times two
// hundred cells per page.
func TestTheGridSizeIsAskedForExplicitly(t *testing.T) {
	if !strings.HasPrefix(thumbnailSizeSuffix, "=") {
		t.Fatalf("the size suffix %q is not in Google's suffix form", thumbnailSizeSuffix)
	}
	if strings.HasSuffix(thumbnailSizeSuffix, "-c") {
		t.Error("the cropping variant returns a bigger file than the grid needs; the grid crops in CSS")
	}
}

// A 403 on a thumbnail means the cookies died, not that the image is gone — and the two have
// completely different remedies. Everything else is worth a retry, not a re-login.
func TestARefusedThumbnailReadsAsASessionProblem(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		err := thumbnailStatusError(&http.Response{StatusCode: status, Header: http.Header{}})
		if !errors.Is(err, ErrSessionRejected) {
			t.Errorf("HTTP %d gave %v, want ErrSessionRejected", status, err)
		}
	}

	err := thumbnailStatusError(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || !httpErr.Retryable() {
		t.Errorf("HTTP 429 gave %v, want a retryable HTTPError", err)
	}
}
