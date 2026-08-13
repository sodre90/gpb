package web

import (
	"log"
	"net/http"
	"net/url"

	"gpb/internal/store"
)

// reviewView is the queue in two halves. They share the needs_review flag but not the verb: a
// photo that has appeared in an album the user picks from is theirs to approve or decline, and
// one Google has lost is theirs only to acknowledge. Answering both with one button would mean
// "approve" deciding to re-download something that no longer exists upstream.
type reviewView struct {
	New  []reviewGroup
	Gone []reviewGroup
}

func (v reviewView) Empty() bool { return len(v.New) == 0 && len(v.Gone) == 0 }

type reviewGroup struct {
	AlbumID string
	Title   string
	Cells   []itemCell
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	view, err := s.reviewView()
	if err != nil {
		log.Printf("web: building the review queue: %v", err)
		http.Error(w, "the review queue is unavailable", http.StatusInternalServerError)
		return
	}

	data := s.page(r, "Review")
	data.Data = view
	render(w, http.StatusOK, "review", data)
}

func (s *Server) reviewView() (reviewView, error) {
	fresh, err := s.store.ItemsNeedingReview(store.StateDiscovered)
	if err != nil {
		return reviewView{}, err
	}
	gone, err := s.store.ItemsNeedingReview(store.StateMissingUpstream)
	if err != nil {
		return reviewView{}, err
	}
	return reviewView{New: groupsFor(fresh), Gone: groupsFor(gone)}, nil
}

// everyCellTicked is the queue's answer before the user gives one. "Yes, all of these" is what
// the page is usually told, and a browser without script has no gesture for ticking two hundred
// boxes, so unticking the exceptions is the cheaper way round; Select all only makes the same
// choice quicker once script is there to offer it.
const everyCellTicked = true

func groupsFor(groups []store.ReviewGroup) []reviewGroup {
	rendered := make([]reviewGroup, 0, len(groups))
	for _, group := range groups {
		cells := make([]itemCell, 0, len(group.Items))
		for _, item := range group.Items {
			cells = append(cells, cellFor(item, everyCellTicked))
		}
		rendered = append(rendered, reviewGroup{
			AlbumID: group.AlbumID,
			Title:   displayTitle(group.Title, group.Kind, group.Owner, group.OwnedByMe),
			Cells:   cells,
		})
	}
	return rendered
}

// handleReviewResolve settles the items the user ticked, in whichever of the three ways the
// button they pressed means. Every one of them clears the flag; what differs is what else it
// does, and "dismiss" and "acknowledge" deliberately do nothing else at all — the file on disk
// is never this page's to touch.
func (s *Server) handleReviewResolve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}

	keys := r.Form["resolve"]
	if len(keys) == 0 {
		http.Redirect(w, r, "/review?notice=Nothing+was+ticked.", http.StatusSeeOther)
		return
	}
	if len(keys) > maxKeysPerRequest {
		http.Error(w, "too many items in one request", http.StatusRequestEntityTooLarge)
		return
	}

	settling, err := s.ticked(keys)
	if err != nil {
		log.Printf("web: checking what the queue is asking about: %v", err)
		http.Error(w, "those items could not be settled", http.StatusInternalServerError)
		return
	}
	if len(settling) == 0 {
		http.Redirect(w, r, "/review?notice=Those+items+have+already+been+settled.",
			http.StatusSeeOther)
		return
	}

	if r.FormValue("decision") == "approve" {
		if err := s.store.SetSelection(settling, true); err != nil {
			log.Printf("web: approving reviewed items: %v", err)
			http.Error(w, "those items could not be approved", http.StatusInternalServerError)
			return
		}
	}

	if err := s.store.ClearNeedsReview(settling); err != nil {
		log.Printf("web: clearing review flags: %v", err)
		http.Error(w, "those items could not be settled", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/review?notice="+resolvedNotice(r.FormValue("decision"), len(settling)),
		http.StatusSeeOther)
}

// ticked drops the keys the queue is not asking about. The form carries whatever it carries, and
// a decision on an item that was never in the queue is not the user's decision at all: 'approve'
// would select a photo for download that no page ever showed them.
func (s *Server) ticked(keys []string) ([]string, error) {
	waiting, err := s.store.KeysWaitingForReview(keys)
	if err != nil {
		return nil, err
	}
	if len(waiting) != len(keys) {
		log.Printf("web: a resolve named %d keys, %d of them waiting for review",
			len(keys), len(waiting))
	}
	return waiting, nil
}

func resolvedNotice(decision string, settled int) string {
	switch decision {
	case "approve":
		return url.QueryEscape(quantity(settled, "item") + " will be backed up on the next run.")
	case "dismiss":
		return url.QueryEscape(quantity(settled, "item") + " left out of the backup.")
	default:
		return url.QueryEscape(quantity(settled, "item") + " acknowledged; the files on disk are untouched.")
	}
}
