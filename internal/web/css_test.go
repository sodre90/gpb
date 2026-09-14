package web

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The stylesheet is one flat namespace shared by every page, so the name a page picks for a class
// is really picked for the whole site. That is how the Now card's rows, given class "progress",
// landed on the albums table's "Backed up" cell and took it out of the table's own layout —
// nothing failed to compile and no test noticed, because the collision only exists in the browser.
//
// So names are handed out by a rule, written at the top of app.css: a component owns its own name
// and every name beginning with it, tones and states are worn beside the name they modify, and a
// short list of utilities means the same thing wherever it lands. This file is the vocabulary. A
// class that is none of the three has no owner, and fails here rather than in somebody's browser.
//
// What this does not catch, so that nobody reads a passing run as proof of a clean sheet:
//
//   - a name assembled inside a template action, since the actions are stripped before the markup
//     is read. The albums table's column classes come out of albumsort.go that way.
//   - whether a rule still has anything to style. A component named here that every page has
//     stopped drawing keeps its block in the stylesheet and its entry below.

// components each own a block of the stylesheet, their own name, and every name that starts with
// it: grid owns grid-cell, stat owns stat-value.
var components = map[string]string{
	"actions":       "a row of controls under or beside something",
	"albums":        "the album table: covers, sort links, per-row backup state",
	"badge":         "a small count or label riding on a title",
	"banner":        "a sentence the page wants read before the rest of it",
	"btn":           "a link that has to look like a button",
	"bundles":       "the shared-photo sets, folded away behind a disclosure",
	"card":          "a panel, drawn by a section, a form or a table",
	"crumbs":        "the way back out of a page",
	"diagnostics":   "the Google page's own details, folded away",
	"empty":         "what a page says when it has nothing to show",
	"grid":          "the item grid, its cells, and the picking gesture over them",
	"kv":            "a key-and-value table",
	"library-form":  "the whole-library selector on the albums page",
	"login-page":    "the sign-in page, which is narrower than everything else",
	"meter":         "a bar that fills",
	"mode-form":     "the backup-mode selector on a row or an album",
	"outcome":       "how a run ended",
	"pager":         "older and newer, under a grid",
	"place":         "the search on the Photos page: a name typed, a box on the map found",
	"progress":      "a row of the Now card: what is being fetched and how far it has got",
	"rail":          "the years down the window's edge and the drag along them, built by timeline.js",
	"runs":          "the activity table",
	"settings-form": "the settings page's rows of label and field",
	"skip-link":     "the link past the nav that only a keyboard ever finds",
	"stat":          "one figure with its label under it",
	"stats":         "a row of stats",
	"state":         "a dot saying whether something is connected or not",
	"thumb":         "a picture that arrives late, shimmering until it does",
	"timeline":      "the grid as one piece: its rows, its month headings, and the floating month",
	"topbar":        "the nav across the top of every page",
	"viewer":        "the full-screen photo dialog, built entirely by viewer.js",
	"vnc":           "the frame the login browser is watched through",
}

// shared names belong to nobody, so they mean the same thing wherever they land: utilities that
// colour or shrink text, and the tones and states worn beside a component's own name.
var shared = map[string]string{
	"arrow":         "a direction glyph, dimmed",
	"auth_required": "an outcome: Google stopped believing us mid-run",
	"bad":           "a state that wants attention",
	"danger":        "a tone: something was lost",
	"date":          "a time, kept on one line",
	"done":          "an item this backup holds",
	"drift":         "an outcome: what Google says it has stopped matching what we hold",
	"error":         "a tone: something failed",
	"failed":        "an item that would not download",
	"gone":          "an item Google no longer has",
	"interrupted":   "an outcome: told to stop, which is not a fault",
	"loading":       "a picture still on its way",
	"muted":         "text that steps back",
	"num":           "a column of figures, both its heading and its cells",
	"ok":            "a state or outcome that needs nothing from anyone",
	"partial":       "an outcome: some of it worked",
	"primary":       "the one control on the page that commits",
	"review":        "an item nobody has decided about yet",
	"small":         "text a size down",
	"sr-only":       "text only a screen reader reads",
	"success":       "a tone: it worked",
	"video":         "an item that moves",
	"warn":          "a tone: something needs attention",
	"wrap":          "long text allowed to break mid-word",
}

var (
	templateAction   = regexp.MustCompile(`{{[^}]*}}`)
	elementWithClass = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9]*)\b[^>]*?class="([^"]*)"`)
	cssComment       = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssClass         = regexp.MustCompile(`\.([a-zA-Z][\w-]*)`)
)

func TestEveryClassNameSaysWhoOwnsIt(t *testing.T) {
	for _, where := range []struct {
		what    string
		classes []string
	}{
		{"the templates", classesInTemplates(t)},
		{"the stylesheet", classesInStylesheet(t)},
	} {
		for _, class := range where.classes {
			if ownerOf(class) == "" {
				t.Errorf("class %q in %s belongs to nobody: name it after the component it is "+
					"part of, or add it to shared if it means the same thing everywhere",
					class, where.what)
			}
		}
	}
}

// A name in the vocabulary that nothing wears any more is an entry the next reader would take for
// a description of the site, and a name the next collision could hide under.
func TestNothingInTheVocabularyIsStale(t *testing.T) {
	worn := append(classesInTemplates(t), classesInStylesheet(t)...)

	for component := range components {
		if !slices.ContainsFunc(worn, func(class string) bool { return ownerOf(class) == component }) {
			t.Errorf("component %q owns nothing any more — drop it and its block from app.css", component)
		}
	}
	for name := range shared {
		if !slices.Contains(worn, name) {
			t.Errorf("shared name %q is worn by nothing — drop it", name)
		}
	}
}

// ownerOf answers which component a class belongs to, "" if none does. A shared name is owned by
// everyone, which for this purpose is the same as being owned.
func ownerOf(class string) string {
	if _, everyones := shared[class]; everyones {
		return class
	}
	if _, own := components[class]; own {
		return class
	}
	for component := range components {
		if strings.HasPrefix(class, component+"-") {
			return component
		}
	}
	return ""
}

// classesInTemplates reads the templates rather than rendered pages on purpose: a page renders
// only the state the test put it in, and a class that appears in one arm of a condition is as
// capable of colliding as one that appears in all of them.
func classesInTemplates(t *testing.T) []string {
	t.Helper()

	templates, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		t.Fatalf("looking for the templates: %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("no templates found to read class names out of")
	}

	var classes []string
	for _, name := range templates {
		body, err := fs.ReadFile(assets, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		markup := templateAction.ReplaceAllString(string(body), " ")
		for _, match := range elementWithClass.FindAllStringSubmatch(markup, -1) {
			classes = append(classes, strings.Fields(match[2])...)
		}
	}
	return classes
}

func classesInStylesheet(t *testing.T) []string {
	t.Helper()

	body, err := fs.ReadFile(assets, "static/app.css")
	if err != nil {
		t.Fatalf("reading the stylesheet: %v", err)
	}

	var classes []string
	for _, match := range cssClass.FindAllStringSubmatch(cssComment.ReplaceAllString(string(body), " "), -1) {
		classes = append(classes, match[1])
	}
	return classes
}
