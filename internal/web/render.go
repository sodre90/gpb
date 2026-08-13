package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"

	"gpb/internal/auth"
	"gpb/internal/store"
)

//go:embed templates static
var assets embed.FS

var pages = parsePages("login", "overview", "albums", "album", "photos", "runs", "review", "settings",
	"restarting", "reauth")

type pageData struct {
	Title         string
	Authenticated bool
	Nav           []navItem
	// NavSrc carries the page the nav was rendered on, so the copy that replaces it still knows
	// which link to mark as the current one.
	NavSrc  string
	Session *sessionBanner
	Error   string
	Notice  string
	Data    any
}

type navItem struct {
	Label   string
	Href    string
	Current bool
	// Waiting is the review queue's size, shown as a pill. Zero renders nothing: a badge that
	// says "0" is a thing to check that turns out not to need checking.
	Waiting int
}

// sessionBanner is the one message that outranks whatever page the user asked for. A daemon
// Google has signed out backs nothing up, and a run stopped by drift will not resume on its
// own, but either can be true while every page carries on looking ordinary — the album list
// serves happily with cached rows and grey thumbnails and says nothing about why.
type sessionBanner struct {
	Level   string
	Message string
	Action  string
}

// page is the frame every authenticated page shares: where the user is, what is waiting, and
// what is wrong account-wide. Handlers fill in Data and the title.
func (s *Server) page(r *http.Request, title string) pageData {
	return pageData{
		Title:         title,
		Authenticated: true,
		Nav:           s.nav(r.URL.Path),
		NavSrc:        navSrc(r.URL.Path),
		Session:       s.sessionBanner(),
		Notice:        r.URL.Query().Get("notice"),
		Error:         r.URL.Query().Get("error"),
	}
}

func (s *Server) nav(path string) []navItem {
	items := []navItem{
		{Label: "Overview", Href: "/"},
		{Label: "Albums", Href: "/albums"},
		{Label: "Photos", Href: "/photos"},
		{Label: "Activity", Href: "/runs"},
		{Label: "Review", Href: "/review", Waiting: s.waitingForReview()},
		{Label: "Google", Href: "/reauth"},
		{Label: "Settings", Href: "/settings"},
	}
	for i, item := range items {
		items[i].Current = item.Href == path ||
			(item.Href != "/" && strings.HasPrefix(path, item.Href))
	}
	return items
}

// waitingForReview is a count per page render, over the same set the review page lists. A
// failure loses the pill rather than the page: the nav is not worth a 500.
func (s *Server) waitingForReview() int {
	waiting, err := s.store.CountNeedingReview()
	if err != nil {
		log.Printf("web: counting the review queue: %v", err)
		return 0
	}
	return waiting
}

func (s *Server) sessionBanner() *sessionBanner {
	switch s.auth.Status().State {
	case auth.StateAuthRequired:
		return &sessionBanner{
			Level:   "danger",
			Message: "Google has signed this session out — backups are paused until you sign in again.",
			Action:  "Open the Google page",
		}
	case auth.StateWarmupFailed:
		return &sessionBanner{
			Level: "warn",
			Message: "The last check of the Google session did not finish, so nothing here knows " +
				"whether backups are working. Signing in again is not the remedy for this one.",
			Action: "Check the session",
		}
	}

	if s.lastOutcome() == store.OutcomeDrift {
		return &sessionBanner{
			Level: "warn",
			Message: "The last run stopped because Google changed something in its responses. " +
				"Nothing already backed up is affected, but new photos are not being fetched.",
			Action: "Check the session",
		}
	}
	return nil
}

func (s *Server) lastOutcome() store.Outcome {
	runs, err := s.store.RecentRuns(1)
	if err != nil || len(runs) == 0 {
		return ""
	}
	return runs[0].Outcome
}

var helpers = template.FuncMap{
	"count":    humanCount,
	"quantity": quantity,
}

// parsePages gives every page the layout and the shared components, so a block used on two
// pages — the grid cell is on both the album grid and the review queue — is written once and
// cannot drift into two versions of itself.
func parsePages(names ...string) map[string]*template.Template {
	parsed := make(map[string]*template.Template, len(names))
	for _, name := range names {
		parsed[name] = template.Must(template.New(name).Funcs(helpers).ParseFS(assets,
			"templates/layout.html", "templates/components.html", "templates/"+name+".html"))
	}
	return parsed
}

// render buffers the whole page so a template failure produces a clean 500 rather than a
// truncated document with a 200 already committed.
func render(w http.ResponseWriter, status int, name string, data pageData) {
	page, ok := pages[name]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}

	var rendered bytes.Buffer
	if err := page.ExecuteTemplate(&rendered, "layout.html", data); err != nil {
		log.Printf("web: rendering %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	rendered.WriteTo(w)
}

// renderBlock produces one named block of a page on its own, for a script that replaces part of
// a page it is already showing. It goes through the same template as a full render, so what the
// script puts in place cannot drift from what a reload would have put there.
func renderBlock(page, block string, data any) (string, error) {
	parsed, ok := pages[page]
	if !ok {
		return "", fmt.Errorf("unknown page %q", page)
	}

	var rendered bytes.Buffer
	if err := parsed.ExecuteTemplate(&rendered, block, data); err != nil {
		return "", err
	}
	return rendered.String(), nil
}
