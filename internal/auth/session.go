package auth

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
)

// wizTokenField is where the Google Photos page shell keeps the batchexecute CSRF token.
const wizTokenField = "SNlM0e"

// wizAccountField holds the signed-in account's gaia id. Every album and shared bundle names
// the person who owns it, and this is the only way to tell "you shared these" from "someone
// shared these with you" — the listings themselves never say which of the two the reader is.
// Its absence is not fatal: it decides a caption, not access.
const wizAccountField = "S06Grb"

var (
	ErrNoToken         = errors.New("page shell carried no batchexecute token")
	ErrNoAccountCookie = errors.New("no signed-in account cookie was present")
)

// accountCookieNames are set only once an account is actually signed in. A logged-out
// visit still gets NID/AEC/SOCS and still lands on photos.google.com — its public landing
// page is not a redirect — so the URL alone cannot tell the two states apart.
var accountCookieNames = map[string]bool{
	"SID":               true,
	"SSID":              true,
	"HSID":              true,
	"APISID":            true,
	"SAPISID":           true,
	"__Secure-1PSID":    true,
	"__Secure-3PSID":    true,
	"__Secure-1PAPISID": true,
	"__Secure-3PAPISID": true,
}

// Session holds live Google credentials. It deliberately implements neither Stringer nor
// json.Marshaler: cookie values and the CSRF token must never reach a log line.
type Session struct {
	cookies   []*http.Cookie
	token     string
	accountID string
	userAgent string
	acquired  time.Time
}

func newSession(raw []*network.Cookie, wiz map[string]any, userAgent string, now time.Time) (*Session, error) {
	token, ok := wizTokenFrom(wiz)
	if !ok {
		return nil, ErrNoToken
	}

	if !hasAccountCookie(raw) {
		return nil, ErrNoAccountCookie
	}

	cookies := make([]*http.Cookie, 0, len(raw))
	for _, cookie := range raw {
		cookies = append(cookies, &http.Cookie{
			Name:     cookie.Name,
			Value:    cookie.Value,
			Domain:   cookie.Domain,
			Path:     cookie.Path,
			Secure:   cookie.Secure,
			HttpOnly: cookie.HTTPOnly,
		})
	}

	accountID, _ := wiz[wizAccountField].(string)

	return &Session{
		cookies:   cookies,
		token:     token,
		accountID: accountID,
		userAgent: userAgent,
		acquired:  now,
	}, nil
}

func hasAccountCookie(cookies []*network.Cookie) bool {
	for _, cookie := range cookies {
		if accountCookieNames[cookie.Name] && cookie.Value != "" {
			return true
		}
	}
	return false
}

func wizTokenFrom(wiz map[string]any) (string, bool) {
	value, ok := wiz[wizTokenField]
	if !ok {
		return "", false
	}
	token, ok := value.(string)
	return token, ok && token != ""
}

func (s *Session) Token() string     { return s.token }
func (s *Session) AccountID() string { return s.accountID }
func (s *Session) UserAgent() string { return s.userAgent }
func (s *Session) Acquired() time.Time {
	return s.acquired
}

func (s *Session) CookieCount() int { return len(s.cookies) }

// Jar builds a cookie jar that preserves each cookie's real domain scoping, which a
// captured Cookie: header cannot express. Content hosts such as
// photos.fife.usercontent.google.com reject the app host's cookies, so correct scoping is
// the difference between a working thumbnail fetch and a redirect to the sign-in page.
func (s *Session) Jar() (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	byHost := make(map[string][]*http.Cookie)
	for _, cookie := range s.cookies {
		host := strings.TrimPrefix(cookie.Domain, ".")
		if host == "" {
			continue
		}
		byHost[host] = append(byHost[host], cookie)
	}

	for host, cookies := range byHost {
		jar.SetCookies(&url.URL{Scheme: "https", Host: host, Path: "/"}, cookies)
	}
	return jar, nil
}
