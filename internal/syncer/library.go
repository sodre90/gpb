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
// limited to the part of the timeline it actually reached — see reconcilableFrom.
func (s *Syncer) listLibrary(ctx context.Context, since time.Time) (int, error) {
	listedAt := time.Now()
	listed := 0
	walkedItAll := false

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
			if err := s.store.LinkItemToAlbum(store.LibraryID, item.MediaKey, listedAt); err != nil {
				return listed, err
			}
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
	}

	log.Printf("syncer: the library walk recorded %d items", listed)
	if err := s.reconcileLibrary(listed, reconcilableFrom(since, walkedItAll), listedAt); err != nil {
		return listed, err
	}
	return listed, s.store.MarkAlbumSynced(store.LibraryID, listedAt)
}

// reconcileLibrary writes off the photos the timeline no longer lists. It is the only thing that
// ever notices a deletion for a photo in no album, and most of a library is in no album.
//
// A walk that listed nothing reconciles nothing. An account under backup is never empty, so a
// timeline that answers with no items is a failure wearing a success's clothes — and taken at its
// word it would write off every photo the user has.
func (s *Syncer) reconcileLibrary(listed int, capturedFrom, listedAt time.Time) error {
	if listed == 0 {
		log.Print("syncer: the library walk listed nothing, so nothing is written off")
		return nil
	}

	departed, err := s.store.ReconcileLibrary(capturedFrom, listedAt)
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

// walkedPastTheBound ends the walk once a whole page has fallen behind the date rather than at
// the first item that has. The timeline is ordered by capture time, but those times carry the
// camera's own timezone, so around the boundary the order can wobble by the better part of a day.
func walkedPastTheBound(page []gphotos.MediaItem, since time.Time) bool {
	return len(page) > 0 && olderThan(page[len(page)-1], since)
}
