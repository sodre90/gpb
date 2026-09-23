package gphotos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	batchExecuteURL = "https://photos.google.com/_/PhotosUi/data/batchexecute"
	signInHost      = "accounts.google.com"
	requestTimeout  = 60 * time.Second
	maxResponseSize = 64 << 20
)

// ErrSessionRejected means the cookies no longer authenticate. It is distinct from drift:
// the remedy is a warmup or an interactive re-login, not a decoder fix.
var ErrSessionRejected = errors.New("google rejected the session")

// Session is the credential set a warmup harvests. gphotos consumes it as an interface so
// the protocol code never imports the browser machinery that produces it.
type Session interface {
	Jar() (http.CookieJar, error)
	Token() string
	AccountID() string
	UserAgent() string
}

// Waiter throttles outbound requests. The sync engine passes one shared token bucket so
// listing and downloading draw from a single budget rather than racing each other.
type Waiter interface {
	Wait(ctx context.Context) error
}

type Client struct {
	http *http.Client
	// downloads shares the session jar but carries no client deadline: requestTimeout bounds a
	// batchexecute call, while a half-gigabyte video outlasts any fixed timeout and is governed
	// by the download context instead.
	downloads *http.Client
	token     string
	accountID string
	userAgent string
	limiter   Waiter
	requests  atomic.Int64
}

func NewClient(session Session) (*Client, error) {
	jar, err := session.Jar()
	if err != nil {
		return nil, fmt.Errorf("building the cookie jar: %w", err)
	}
	if session.Token() == "" {
		return nil, fmt.Errorf("%w: the session carries no batchexecute token", ErrSessionRejected)
	}

	return &Client{
		http:      &http.Client{Jar: jar, Timeout: requestTimeout},
		downloads: &http.Client{Jar: jar},
		token:     session.Token(),
		accountID: session.AccountID(),
		userAgent: session.UserAgent(),
	}, nil
}

// AccountID is the gaia id of the signed-in account, or empty if the page shell did not carry
// it. Callers use it to recognise the account holder among the people the listings name.
func (c *Client) AccountID() string { return c.accountID }

// Throttle installs a shared limiter. Every batchexecute call waits on it; downloads wait
// on the same bucket from the sync engine, which is what keeps total request rate boring.
func (c *Client) Throttle(limiter Waiter) {
	c.limiter = limiter
}

func (c *Client) call(ctx context.Context, rpcID string, payload []any, sourcePath string) (any, error) {
	body, err := encodeRequest(rpcID, payload)
	if err != nil {
		return nil, err
	}

	raw, err := c.post(ctx, c.callURL(rpcID, sourcePath), body)
	if err != nil {
		return nil, err
	}

	frames, err := decodeFrames(raw)
	if err != nil {
		return nil, err
	}
	return payloadFor(frames, rpcID)
}

// callURL mirrors what the web app sends. rt=c selects the chunked framing the decoder
// expects, and source-path tells the server which surface is asking — album reads are
// refused without it.
func (c *Client) callURL(rpcID, sourcePath string) string {
	query := url.Values{}
	query.Set("rpcids", rpcID)
	query.Set("rt", "c")
	query.Set("_reqid", strconv.FormatInt(c.nextRequestID(), 10))
	query.Set("hl", "en")
	if sourcePath != "" {
		query.Set("source-path", sourcePath)
	}
	return batchExecuteURL + "?" + query.Encode()
}

func (c *Client) nextRequestID() int64 {
	return 100000 + c.requests.Add(1)*100000
}

func (c *Client) post(ctx context.Context, endpoint, body string) (string, error) {
	if err := c.wait(ctx); err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("f.req", body)
	form.Set("at", c.token)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	request.Header.Set("User-Agent", c.userAgent)
	request.Header.Set("Origin", "https://photos.google.com")
	request.Header.Set("X-Same-Domain", "1")

	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("batchexecute request: %w", withoutAddress(err))
	}
	defer response.Body.Close()

	if landedOnSignIn(response) {
		return "", fmt.Errorf("%w: the request was redirected to the sign-in page", ErrSessionRejected)
	}
	if response.StatusCode != http.StatusOK {
		return "", statusError(response)
	}

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return "", fmt.Errorf("reading the batchexecute response: %w", err)
	}
	return string(payload), nil
}

func (c *Client) wait(ctx context.Context) error {
	if c.limiter == nil {
		return nil
	}
	return c.limiter.Wait(ctx)
}

func landedOnSignIn(response *http.Response) bool {
	return response.Request != nil && response.Request.URL != nil &&
		strings.Contains(response.Request.URL.Host, signInHost)
}

// statusError maps the codes worth reacting to differently. 401/403 mean the session died
// mid-run, which the engine reports as auth_required rather than retrying into a wall.
func statusError(response *http.Response) error {
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: HTTP %d", ErrSessionRejected, response.StatusCode)
	default:
		return &HTTPError{StatusCode: response.StatusCode, RetryAfter: retryAfter(response)}
	}
}

// HTTPError carries the two things the backoff policy needs: whether the failure is worth
// retrying and how long the server asked us to wait.
type HTTPError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("google photos returned HTTP %d, retry after %s", e.StatusCode, e.RetryAfter)
	}
	return fmt.Sprintf("google photos returned HTTP %d", e.StatusCode)
}

// Retryable treats 429 and the 5xx family as transient. Everything else is a bug or a
// permanent refusal, and retrying it just burns the rate budget.
func (e *HTTPError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

func retryAfter(response *http.Response) time.Duration {
	header := response.Header.Get("Retry-After")
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		if delay := time.Until(at); delay > 0 {
			return delay
		}
	}
	return 0
}

// withoutAddress keeps what failed and why, and drops the address it failed at. net/http puts
// the whole request URL into its errors, and for a download that URL is signed: whoever reads
// the log line or the item's recorded error could fetch the photograph with it.
func withoutAddress(err error) error {
	var requestErr *url.Error
	if !errors.As(err, &requestErr) {
		return err
	}
	return fmt.Errorf("%s to %s: %w", requestErr.Op, hostOnly(requestErr.URL), requestErr.Err)
}

func hostOnly(address string) string {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" {
		return "an address that does not parse"
	}
	return parsed.Host
}
