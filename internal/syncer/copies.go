package syncer

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gpb/internal/store"
)

// linkOver makes path a second name for the file at held, replacing whatever path was. The link
// is made under a temporary name in the staging directory and renamed over path, so path is at
// every moment either the file it was or the held one — never missing, never half written — and a
// crash between the two leaves its litter where the next run tidies, not in the pool. Both names
// are real files to everything else in gpb: removing either leaves the other, which is what lets
// the written-off-copy cleanup and a verify repair go on working on one name at a time.
func linkOver(held, path, stagingDir string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("preparing the pool directory: %w", err)
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return fmt.Errorf("preparing the staging directory: %w", err)
	}
	staged := filepath.Join(stagingDir, rand.Text()+stagedLinkExtension)
	if err := os.Link(held, staged); err != nil {
		return fmt.Errorf("linking to the held copy: %w", err)
	}
	if err := os.Rename(staged, path); err != nil {
		os.Remove(staged)
		return fmt.Errorf("putting the link in place: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

// commitSharingAHeldCopy commits a download as a link to a file already holding the same bytes,
// and reports whether it did. Anything that stands in the way — no held copy, one that no longer
// hashes to its record, a filesystem that will not link — leaves the caller to commit the fresh
// bytes as a file of their own, which is never worse than what a run did before this existed.
func (s *Syncer) commitSharingAHeldCopy(item store.MediaItem, result downloaded, part, final string) bool {
	held, found, err := s.store.HeldCopyOf(result.SHA256, item.MediaKey)
	if err != nil || !found || held.SizeBytes != result.Size {
		return false
	}
	if refusal := keptCopyIsIntact(held); refusal != "" {
		log.Printf("syncer: not sharing a held copy of an item: %s", refusal)
		return false
	}
	if err := linkOver(held.LocalPath, final, s.tempDir); err != nil {
		log.Printf("syncer: keeping a second copy of an item rather than a link: %v", err)
		return false
	}
	if err := os.Remove(part); err != nil {
		log.Printf("syncer: removing a download committed as a link: %v", err)
	}
	return true
}

// Linking is what one pass of `gpb duplicates --link` found and did.
type Linking struct {
	Linking  bool
	Photos   int
	Separate int
	Linked   int
	Freed    int64
	Held     int64
	Refusals []string
}

func (l Linking) String() string {
	var parts []string
	switch {
	case l.Separate == 0:
		parts = append(parts, fmt.Sprintf("%d photos are held under more than one key, and each is already one file", l.Photos))
	case l.Linking:
		parts = append(parts, fmt.Sprintf("%d copies made links to the file they repeated, %s freed", l.Linked, humanBytes(l.Freed)))
	default:
		parts = append(parts, fmt.Sprintf("%d photos are kept as more than one file, %d extra files, %s; --link makes each one file",
			l.Photos, l.Separate, humanBytes(l.Held)))
	}
	if len(l.Refusals) > 0 {
		parts = append(parts, fmt.Sprintf("%d left as they are because a copy could not be verified", len(l.Refusals)))
	}
	return strings.Join(parts, ", ")
}

const stagedLinkExtension = ".link"

// CountCopies finds the photographs held as more than one file under different keys, and reads
// none of them.
func CountCopies(ctx context.Context, db *store.Store) (Linking, error) {
	return copies(ctx, db, "", false)
}

// LinkCopies makes every copy CountCopies would find a hardlink to the first. Both files are
// re-read first and must hash to what was recorded: linking over a good file with a rotted one
// would turn one bad copy into two. Files already sharing an inode are left alone, so a pass
// interrupted part way is finished by the next.
func LinkCopies(ctx context.Context, db *store.Store, stagingDir string) (Linking, error) {
	return copies(ctx, db, stagingDir, true)
}

func copies(ctx context.Context, db *store.Store, stagingDir string, link bool) (Linking, error) {
	groups, err := db.SameBytesGroups()
	if err != nil {
		return Linking{}, err
	}

	linking := Linking{Linking: link}
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return linking, err
		}
		separate := separateFiles(group[0], group[1:])
		if len(separate) == 0 {
			continue
		}
		linking.Photos++
		linking.Separate += len(separate)
		linking.Held += int64(len(separate)) * group[0].SizeBytes
		if !link {
			continue
		}
		linked, freed, refusals := linkGroup(group[0], separate, stagingDir)
		linking.Linked += linked
		linking.Freed += freed
		linking.Refusals = append(linking.Refusals, refusals...)
	}
	return linking, nil
}

// separateFiles is the copies that are still files of their own rather than names for kept's.
// A copy that cannot be looked at is counted as separate, and verification turns it away.
func separateFiles(kept store.MediaItem, copies []store.MediaItem) []store.MediaItem {
	keptInfo, keptErr := os.Stat(kept.LocalPath)
	var separate []store.MediaItem
	for _, repeat := range copies {
		info, err := os.Stat(repeat.LocalPath)
		if keptErr == nil && err == nil && os.SameFile(keptInfo, info) {
			continue
		}
		separate = append(separate, repeat)
	}
	return separate
}

func linkGroup(kept store.MediaItem, copies []store.MediaItem, stagingDir string) (int, int64, []string) {
	if refusal := keptCopyIsIntact(kept); refusal != "" {
		return 0, 0, []string{kept.LocalPath + ": " + refusal}
	}
	linked, freed := 0, int64(0)
	var refusals []string
	for _, repeat := range copies {
		if refusal := keptCopyIsIntact(repeat); refusal != "" {
			refusals = append(refusals, repeat.LocalPath+": "+refusal)
			continue
		}
		if err := linkOver(kept.LocalPath, repeat.LocalPath, stagingDir); err != nil {
			refusals = append(refusals, repeat.LocalPath+": "+err.Error())
			continue
		}
		linked++
		freed += repeat.SizeBytes
	}
	return linked, freed, refusals
}
