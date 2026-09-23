package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"gpb/internal/gphotos"
	"gpb/internal/store"
)

// RefreshAlbums records every album Google reports, downloading nothing. A run needs this to
// know what it follows, and the album picker needs it to offer a current picture of what
// there is to follow, so it is worth doing on its own.
// It reads two listings because neither is the library on its own. The album listing carries
// albums this account owns plus nameless bundles of shared photos; the shared listing repeats
// those albums and adds the ones other people shared, which the first listing omits entirely.
func (s *Syncer) RefreshAlbums(ctx context.Context) error {
	_, err := s.refreshAlbums(ctx)
	return err
}

// albumRefresh is what one pass over the album listings amounts to. The instant is the dividing
// line a walk needs: every album Google still knows about carries it in last_seen_at afterwards,
// and one that does not is an album Google has stopped listing. The count is how much that
// dividing line can be trusted — an album missing from a listing of two hundred has gone, while
// an album missing from a listing of none says only that the listing came back empty.
type albumRefresh struct {
	at       time.Time
	recorded int
}

func (s *Syncer) refreshAlbums(ctx context.Context) (albumRefresh, error) {
	refresh := albumRefresh{at: time.Now()}

	owned, err := s.recordListing(ctx, s.source.Albums, refresh.at, nil)
	if err != nil {
		return refresh, fmt.Errorf("listing albums: %w", err)
	}
	// The shared listing's own entries do not say which of the two they are — an album this
	// account owns looks exactly like one shared with it — so the album listing having already
	// claimed an id is what separates them.
	shared, err := s.recordListing(ctx, s.source.SharedAlbums, refresh.at, owned)
	if err != nil {
		return refresh, fmt.Errorf("listing shared albums: %w", err)
	}

	refresh.recorded = len(owned) + len(shared)
	return refresh, nil
}

type listing func(ctx context.Context, pageToken string) (gphotos.AlbumPage, error)

// recordListing walks one paginated listing, skipping ids already claimed, and reports every
// id it saw so a later listing can tell new albums from repeats.
func (s *Syncer) recordListing(ctx context.Context, fetch listing,
	seenAt time.Time, claimed map[string]bool) (map[string]bool, error) {

	seen := map[string]bool{}
	for token := ""; ; {
		page, err := fetch(ctx, token)
		if err != nil {
			return nil, err
		}
		for _, album := range page.Albums {
			if claimed[album.ID] {
				continue
			}
			if album.Kind == "" {
				album.Kind = gphotos.AlbumShared
			}
			if err := s.store.UpsertAlbum(toStoreAlbum(album, s.source.AccountID()), seenAt); err != nil {
				return nil, err
			}
			seen[album.ID] = true
		}
		if token = page.NextToken; token == "" {
			return seen, nil
		}
	}
}

// list refreshes every album, then walks the followed ones and, if it is followed, the library.
//
// Every run walks everything again rather than resuming where the last one stopped, and that is
// deliberate: seeing all of an album is what lets listAlbum infer that the items it did not see
// have gone from Google. A resumable walk would be faster and would never notice a deletion.
//
// Each phase is timed because the run log used to print one total at the end, which said nothing
// about where a long run spent its time.
func (s *Syncer) list(ctx context.Context) (listingResult, error) {
	startedAt := time.Now()
	refresh, err := s.refreshAlbums(ctx)
	if err != nil {
		return listingResult{}, err
	}

	followed, err := s.store.FollowedAlbums()
	if err != nil {
		return listingResult{}, err
	}
	log.Printf("syncer: %d albums to walk, listed in %s", len(followed), since(startedAt))

	albumsStartedAt := time.Now()
	walk, err := s.walkAlbums(ctx, followed, refresh)
	result := walk.result()
	if err != nil {
		return result, err
	}
	albumTime := since(albumsStartedAt)

	library, err := s.store.Library()
	if err != nil {
		return result, err
	}
	if library.SyncMode == store.SyncNone {
		log.Printf("syncer: listing finished in %s — %d albums in %s, the library is not followed",
			since(startedAt), len(followed), albumTime)
		return result, nil
	}

	libraryStartedAt := time.Now()
	count, err := s.listLibrary(ctx, library.Since)
	log.Printf("syncer: listing finished in %s — %d albums in %s, the library walk in %s",
		since(startedAt), len(followed), albumTime, since(libraryStartedAt))
	result.listed += count
	return result, err
}

// listingResult is what the listing half of a run reports back: how much it recorded, and the
// albums it did not read. The two kinds of unread album are kept apart because they ask different
// things of the user — an album that will not decode is a bug to be reported, while an album
// Google no longer lists is one to stop following.
type listingResult struct {
	listed   int
	skipped  []string
	unlisted []string
}

// albumWalk is the album half of that, plus what it takes to judge the walk as a whole.
type albumWalk struct {
	listed   int
	walked   int
	skipped  []string
	unlisted []string
	drifted  error
	attempts int
	// listingWasEmpty records that Google named no albums at all this run, which is the difference
	// between albums that have gone and a listing not worth believing.
	listingWasEmpty bool
}

func (w albumWalk) result() listingResult {
	return listingResult{listed: w.listed, skipped: w.skipped, unlisted: w.unlisted}
}

// walkAlbums lists every followed album, stepping over the two kinds it cannot usefully read.
//
// The first is an album Google has stopped listing — deleted, or a share withdrawn. Following
// outlives listing: nothing clears sync_mode when an album goes, so its id is asked for on every
// run forever, and the answer is not a listing of anything. The refresh that just ran is what
// says so, and skipping costs one request less per run rather than one more.
//
// The second is an album whose contents will not decode. A single one used to cost the entire
// nightly backup, including every album after it in the walk order and the library timeline
// behind them. Stepping over it is safe because listAlbum returns before reconciling: a skipped
// album keeps every link it has and merely goes another day unrefreshed.
//
// Only drift is stepped over among failures. A rejected session means the account is signed out
// and the next four hundred albums would fail identically; a cancelled context means the daemon
// is stopping. Neither improves by being asked again four hundred times.
func (s *Syncer) walkAlbums(ctx context.Context, followed []store.Album, refresh albumRefresh) (albumWalk, error) {
	walk := albumWalk{listingWasEmpty: refresh.recorded == 0}
	for _, album := range followed {
		if album.LastSeenAt.Before(refresh.at) {
			walk.unlisted = append(walk.unlisted, describeAlbum(album))
			continue
		}

		walkStartedAt := time.Now()
		count, err := s.listAlbum(ctx, album.ID)
		walk.listed += count
		walk.attempts++

		switch {
		case err == nil:
			walk.walked++
			log.Printf("syncer: an album walk recorded %d items in %s", count, since(walkStartedAt))
		case errors.Is(err, errAlbumCountContradicted):
			return walk, err
		case errors.Is(err, gphotos.ErrProtocolDrift):
			walk.skipped = append(walk.skipped, s.describeAlbum(album.ID))
			if walk.drifted == nil {
				walk.drifted = err
			}
			log.Printf("syncer: leaving an album as it is and walking on: %v", err)
		default:
			return walk, err
		}
	}
	return walk, walk.verdict()
}

// verdict separates one album Google answers strangely from a protocol that has changed
// underneath the whole run. If nothing decoded, the decoders are wrong rather than the album,
// and a run that read nothing has to say so: reported as a success it would look like an account
// with no photos in it, which is the one reading the strict decoders exist to prevent. Whether
// anything decoded is the whole rule — there is no threshold to tune and no count to get wrong.
//
// An album listing that named nothing at all gets the same treatment and for the same reason.
// A followed album missing from a listing of two hundred has gone, and not walking it is right;
// a followed album missing from a listing of none says only that the listing came back empty,
// and believed, it would leave the user with a backup that had quietly stopped walking anything.
// The count of albums Google named is what tells those apart — not the count that went missing,
// which is the same "all of them" in both.
func (w albumWalk) verdict() error {
	if w.drifted != nil && w.walked == 0 {
		return w.drifted
	}
	if w.listingWasEmpty && len(w.unlisted) > 0 {
		return fmt.Errorf("google's album listing named no albums at all, so none of the %d "+
			"this account follows could be walked", len(w.unlisted))
	}
	w.reportWhatItStepped()
	return nil
}

func (w albumWalk) reportWhatItStepped() {
	switch len(w.skipped) {
	case 0:
	case 1:
		log.Printf("syncer: 1 of %d albums could not be listed and was left as it was", w.attempts)
	default:
		log.Printf("syncer: %d of %d albums could not be listed and were left as they were",
			len(w.skipped), w.attempts)
	}

	switch len(w.unlisted) {
	case 0:
	case 1:
		log.Print("syncer: 1 followed album is no longer listed by Google and was not walked")
	default:
		log.Printf("syncer: %d followed albums are no longer listed by Google and were not walked",
			len(w.unlisted))
	}
}

// since rounds to the second because these are phases measured in minutes and nobody reading a
// run log needs the nanoseconds.
func since(startedAt time.Time) time.Duration {
	return time.Since(startedAt).Round(time.Second)
}

// ListAlbum records one album's contents without downloading anything. The item grid needs
// it: an album nobody follows has never been walked, so there is nothing to pick from until
// someone asks Google for its contents.
func (s *Syncer) ListAlbum(ctx context.Context, albumID string) (int, error) {
	return s.listAlbum(ctx, albumID)
}

// listAlbum pages one album to the end and then reconciles: anything previously linked but
// not seen in this pass has gone from the album upstream. The full walk is what makes that
// inference safe, so a partial listing must never reach the reconciliation step.
//
// That early return is load-bearing beyond this function: walkAlbums steps over an album whose
// contents will not decode, and it is only safe to do so because a listing that failed leaves
// the album's links exactly as it found them.
func (s *Syncer) listAlbum(ctx context.Context, albumID string) (int, error) {
	listedAt := time.Now()
	listed := 0

	alreadyIn, err := s.membersToCompareAgainst(albumID)
	if err != nil {
		return listed, err
	}
	var arrived []string

	for token := ""; ; {
		page, err := s.source.AlbumItems(ctx, albumID, token)
		if err != nil {
			return listed, fmt.Errorf("listing the items of %s: %w", s.describeAlbum(albumID), err)
		}

		for _, item := range page.Items {
			if err := s.store.UpsertItem(toStoreItem(item), listedAt); err != nil {
				return listed, err
			}
			if err := s.store.LinkItemToAlbum(albumID, item.MediaKey, listedAt); err != nil {
				return listed, err
			}
			if alreadyIn != nil && !alreadyIn[item.MediaKey] {
				arrived = append(arrived, item.MediaKey)
			}
			listed++
			s.live.listed.Add(1)
		}

		if token = page.NextToken; token == "" {
			break
		}
	}

	if err := s.checkTheWalkAgreesWithGoogle(albumID, listed); err != nil {
		return listed, err
	}

	if err := s.flagArrivals(albumID, arrived); err != nil {
		return listed, err
	}

	departed, err := s.store.ReconcileAlbum(albumID, listedAt)
	if err != nil {
		return listed, err
	}
	if departed.LeftTheAlbum > 0 {
		log.Printf("syncer: %d items left an album, %d of them gone from Google%s",
			departed.LeftTheAlbum, departed.GoneFromGoogle, copiesNote(departed))
	}

	return listed, s.store.MarkAlbumSynced(albumID, listedAt)
}

// copiesNote says how many of the items written off were byte-identical copies of photos
// Google still has, which the review queue is not asked about.
func copiesNote(departed store.Departures) string {
	if departed.Copies == 0 {
		return ""
	}
	return fmt.Sprintf(", %d of those identical copies of photos still there", departed.Copies)
}

// describeAlbum names a failing album in the terms that tell its failures apart. The id alone
// would say which row broke without saying anything about why, and the three ways an album
// listing fails look identical in the decoder: an album Google has stopped listing was last seen
// before this run started, an empty album was seen just now holding nothing, and real protocol
// drift was seen just now holding something. The title is here because it is what the person
// reading the log calls the album, and the id because it is what `gpb unfollow` takes.
// errAlbumCountContradicted is drift arriving by a route no decoder can refuse: the page parses
// cleanly and simply holds no entries, for an album Google's own listing says is not empty. The
// likeliest cause is the entries moving to another slot, which every album with contents would
// hit at once — so it stops the walk rather than being stepped over like one strange album.
var errAlbumCountContradicted = fmt.Errorf(
	"%w: an album page carried no items at all for an album Google says is not empty",
	gphotos.ErrProtocolDrift)

// checkTheWalkAgreesWithGoogle refuses to let a walk that listed nothing write everything off.
// reconcileLibrary has the same rule for a different reason: an account under backup is never
// empty, so it can treat an empty listing as a failure outright. An album may perfectly well
// hold nothing, so the rule here is not "a walk that listed nothing is wrong" but "Google told
// us in this run's album listing whether this album is empty, and the walk has to agree with it".
func (s *Syncer) checkTheWalkAgreesWithGoogle(albumID string, listed int) error {
	if listed > 0 {
		return nil
	}
	album, err := s.store.Album(albumID)
	if err != nil {
		return err
	}
	if album.ItemCount == 0 {
		return nil
	}
	return fmt.Errorf("%w — %s", errAlbumCountContradicted, describeAlbum(album))
}

func (s *Syncer) describeAlbum(albumID string) string {
	album, err := s.store.Album(albumID)
	if err != nil {
		return fmt.Sprintf("album %s", albumID)
	}
	return describeAlbum(album)
}

func describeAlbum(album store.Album) string {
	held := fmt.Sprintf("%d items", album.ItemCount)
	if album.ItemCount == 1 {
		held = "1 item"
	}
	return fmt.Sprintf("%s (%s), which Google last listed as holding %s on %s",
		nameOf(album), album.ID, held, album.LastSeenAt.Format(time.RFC3339))
}

// nameOf says what to call an album in a sentence. Kind carries the name where a title cannot: a
// bundle has no title in any response Google serves, and an album may simply not have one, so an
// empty pair of quotes would be the least informative thing to print. internal/web says the same
// at more length for the pages, and the two are free to differ — a log line is not a table cell.
func nameOf(album store.Album) string {
	if album.Title == "" {
		return "the untitled " + kindNoun(album.Kind)
	}
	return fmt.Sprintf("the %s %q", kindNoun(album.Kind), album.Title)
}

func kindNoun(kind store.AlbumKind) string {
	switch kind {
	case store.AlbumShared:
		return "shared album"
	case store.AlbumBundle:
		return "bundle of shared photos"
	case store.AlbumLibrary:
		return "library"
	default:
		return "album"
	}
}

// membersToCompareAgainst returns what the album held before this listing, or nil when nothing
// arriving in it could need a decision. Two albums answer nil: one synced 'all' or 'none', where
// a new item is downloaded or ignored without anyone being asked, and one never walked before,
// where every item would look new and the user would be handed a review queue the size of the
// album for merely having pressed refresh. The first walk is the baseline, not a change to it.
func (s *Syncer) membersToCompareAgainst(albumID string) (map[string]bool, error) {
	album, err := s.store.Album(albumID)
	if err != nil {
		return nil, err
	}
	if album.SyncMode != store.SyncPicked || album.LastSyncedAt.IsZero() {
		return nil, nil
	}
	return s.store.MembersOf(albumID)
}

// flagArrivals holds back items that turned up in a 'picked' album since it was last walked.
// DESIGN.md §4: they are recorded but not downloaded, because the user picks from a 'picked'
// album and nobody has picked these yet.
func (s *Syncer) flagArrivals(albumID string, arrived []string) error {
	if len(arrived) == 0 {
		return nil
	}
	if err := s.store.FlagForReview(arrived); err != nil {
		return err
	}
	log.Printf("syncer: %d new items in a picked album are waiting for a decision", len(arrived))
	return nil
}

// toStoreAlbum resolves the owner against the signed-in account here, because this is the only
// layer that holds both. An empty accountID — a page shell that stopped carrying it — leaves
// every album owned by someone named rather than silently claiming they are all the user's.
func toStoreAlbum(album gphotos.Album, accountID string) store.Album {
	return store.Album{
		ID:             album.ID,
		Title:          album.Title,
		ItemCount:      album.ItemCount,
		CreatedAt:      album.CreatedAt,
		Kind:           store.AlbumKind(album.Kind),
		CoverURL:       album.CoverURL,
		OwnerName:      album.Owner.Name,
		OwnerIsAccount: accountID != "" && album.Owner.ID == accountID,
	}
}

// toStoreItem leaves the filename empty on purpose. Google's listing does not carry one — the
// name arrives with the download, in its Content-Disposition — and standing the media key in its
// place made every page that shows a name show an identifier instead.
func toStoreItem(item gphotos.MediaItem) store.MediaItem {
	return store.MediaItem{
		MediaKey:     item.MediaKey,
		CapturedAt:   item.LocalCaptureTime(),
		ThumbnailURL: item.ThumbnailURL,
		IsVideo:      item.IsVideo,
	}
}
