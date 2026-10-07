package syncer

import (
	"context"
	"fmt"
	"log"
	"time"

	"gpb/internal/gphotos"
	"gpb/internal/store"
)

// listLibrary walks the timeline — everything the account holds, newest capture first — down to
// the date the user set. It is the only way to reach a photo that is in no album, which on a
// real library is most of them.
//
// Unlike an album walk, this one usually stops early by design, so what it reconciles has to be
// limited to the part of the timeline it actually reached — see reconcilableFrom. It stops at the
// date bound, and when the user has asked it to look only for what is new, at the first page the
// library already held: on a night with a dozen new photos that is one page in place of 175.
func (s *Syncer) listLibrary(ctx context.Context, library store.Album) (int, error) {
	listedAt := time.Now()
	since := library.Since
	newOnly := walksNewOnly(library, listedAt)
	if library.NewOnly && !newOnly {
		log.Print("syncer: walking the whole library tonight, as it does once a week")
	}

	listed := 0
	walkedItAll := false
	var heldFrom time.Time
	seen := map[string]bool{}

	members, err := s.store.AlbumMembers(store.LibraryID)
	if err != nil {
		return 0, err
	}

	for token := ""; ; {
		page, err := s.source.Timeline(ctx, token)
		if err != nil {
			return listed, fmt.Errorf("listing the library: %w", err)
		}

		for _, item := range page.Items {
			if olderThan(item, since) {
				continue
			}
			if err := s.store.UpsertItem(toStoreItem(item), listedAt); err != nil {
				return listed, err
			}
			if err := s.store.LinkItemToAlbum(store.LibraryID, item.MediaKey); err != nil {
				return listed, err
			}
			seen[item.MediaKey] = true
			listed++
			s.live.listed.Add(1)
		}

		if token = page.NextToken; token == "" {
			walkedItAll = true
			break
		}
		if walkedPastTheBound(page.Items, since) {
			break
		}
		if reached, held := knownGround(page.Items, members); newOnly && held {
			heldFrom = reached
			break
		}
	}

	vouchedFrom := reconcilableFrom(since, walkedItAll)
	if heldFrom.IsZero() {
		log.Printf("syncer: the library walk recorded %d items", listed)
	} else {
		log.Printf("syncer: the library walk recorded %d items and stopped at photos it already held, taken %s and before",
			listed, heldFrom.Format(time.DateOnly))
		vouchedFrom = later(vouchedFrom, reconcilableFrom(heldFrom, false))
	}

	gone := departures(members, seen, vouchedFrom)
	if err := s.reconcileLibrary(listed, gone, listedAt); err != nil {
		return listed, err
	}
	if heldFrom.IsZero() {
		if err := s.store.MarkLibraryWalkedInFull(listedAt); err != nil {
			return listed, err
		}
	}
	return listed, s.store.MarkAlbumSynced(store.LibraryID, listedAt)
}

// fullWalkInterval is how often a library that is walked only for what is new is walked whole
// instead. A week less half a day, because the library walk starts at a different minute each
// night — after however long the albums took, 7 minutes one night and 23 the next on the box this
// was built against — and a strict seven days would slip to the eighth night whenever a walk
// started earlier than the last.
const fullWalkInterval = 7*24*time.Hour - 12*time.Hour

// walksNewOnly is whether tonight's walk may stop at the first page the library already holds:
// only if the user asked for that, and no whole walk is owed. A library never walked whole is owed
// one whatever the weekly setting, and so is one whose date or mode has changed since — SetLibrary
// forgets the last whole walk then, because what the change asks for lies below where a walk
// looking only for what is new would stop.
func walksNewOnly(library store.Album, now time.Time) bool {
	switch {
	case !library.NewOnly, library.WalkedInFullAt.IsZero():
		return false
	case !library.WeeklyFullWalk:
		return true
	default:
		return now.Sub(library.WalkedInFullAt) < fullWalkInterval
	}
}

// knownGround is where a walk looking only for what is new may stop: a page every item of which
// the library held when the walk began. It answers the oldest capture on the page, which is as far
// down as the walk can vouch for. A page with no dated item cannot say where in the timeline it
// is, so it is no ground to stop on.
func knownGround(page []gphotos.MediaItem, members map[string]time.Time) (time.Time, bool) {
	var oldest time.Time
	for _, item := range page {
		if _, held := members[item.MediaKey]; !held {
			return time.Time{}, false
		}
		if captured := item.LocalCaptureTime(); !captured.IsZero() && (oldest.IsZero() || captured.Before(oldest)) {
			oldest = captured
		}
	}
	return oldest, !oldest.IsZero()
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// reconcileLibrary writes off the photos the timeline no longer lists. It is the only thing that
// ever notices a deletion for a photo in no album, and most of a library is in no album.
//
// A walk that listed nothing reconciles nothing. An account under backup is never empty, so a
// timeline that answers with no items is a failure wearing a success's clothes — and taken at its
// word it would write off every photo the user has.
func (s *Syncer) reconcileLibrary(listed int, gone []string, listedAt time.Time) error {
	if listed == 0 {
		log.Print("syncer: the library walk listed nothing, so nothing is written off")
		return nil
	}

	departed, err := s.store.Reconcile(store.LibraryID, gone, listedAt)
	if err != nil {
		return err
	}
	if departed.LeftTheAlbum > 0 {
		log.Printf("syncer: %d items left the library, %d of them gone from Google%s",
			departed.LeftTheAlbum, departed.GoneFromGoogle, copiesNote(departed))
	}
	return nil
}

// boundaryWobble is how far out of order a capture time can sit around the date bound. The times
// carry the camera's own timezone, so the same instant is worth a day either way in the order
// Google serves — see walkedPastTheBound, which reads the same wobble from the other side.
const boundaryWobble = 48 * time.Hour

// reconcilableFrom is the oldest capture date a walk can be believed about. One that reached the
// end of the timeline saw everything above the bound; one that stopped at the bound saw
// everything except what the wobble there could have pushed onto a page it never asked for, so it
// keeps that far clear of it. Below the bound nothing is reconcilable at all: the walk skipped
// those items rather than failing to find them.
func reconcilableFrom(since time.Time, walkedItAll bool) time.Time {
	if walkedItAll {
		return since
	}
	return since.Add(boundaryWobble)
}

// olderThan applies the library's date bound. An item whose capture date Google did not report
// is never older than anything: the bound is there to stop a walk into the archive, not to
// quietly drop the photos with the worst metadata.
func olderThan(item gphotos.MediaItem, since time.Time) bool {
	captured := item.LocalCaptureTime()
	return !since.IsZero() && !captured.IsZero() && captured.Before(since)
}

// departures is what a walk was obliged to see and did not: the album as it stood when the walk
// started, less everything the walk went past. This is the whole of what a listing proves about an
// album, and it costs a walk nothing to say — it is holding both sets already.
func departures(members map[string]time.Time, seen map[string]bool, vouchedFrom time.Time) []string {
	var gone []string
	for key, captured := range members {
		if seen[key] || !reconcilable(captured, vouchedFrom) {
			continue
		}
		gone = append(gone, key)
	}
	return gone
}

// reconcilable says whether a walk that reached back to vouchedFrom was obliged to see an item
// captured at the given moment. A zero floor is a walk that reached the end of the album and
// vouches for everything. An item with no capture date is not within any floor: the order is that
// date, so an item Google never gave one has no known place in it and cannot be shown to have been
// passed over.
//
// The undated case is its own branch although the zero time would fall before any floor anyway,
// because that is a coincidence of how Go spells "no date" and the rule should not rest on it.
func reconcilable(captured, vouchedFrom time.Time) bool {
	switch {
	case vouchedFrom.IsZero():
		return true
	case captured.IsZero():
		return false
	default:
		return !captured.Before(vouchedFrom)
	}
}

// walkedPastTheBound ends the walk once a whole page has fallen behind the date rather than at
// the first item that has. The timeline is ordered by capture time, but those times carry the
// camera's own timezone, so around the boundary the order can wobble by the better part of a day.
func walkedPastTheBound(page []gphotos.MediaItem, since time.Time) bool {
	return len(page) > 0 && olderThan(page[len(page)-1], since)
}
