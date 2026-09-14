package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"gpb/internal/store"
)

// Removal is one written-off file the duplicates command looked at: the twin it is a copy of,
// and either what was done with it or why it was left alone.
type Removal struct {
	Twin    store.Twin
	Removed bool
	Refusal string
}

func (r Removal) String() string {
	twin := r.Twin
	line := fmt.Sprintf("%s (%s) %s %s (%s)",
		twin.WrittenOff.LocalPath, humanBytes(twin.WrittenOff.SizeBytes),
		r.relation(), twin.Kept.LocalPath, humanBytes(twin.Kept.SizeBytes))
	switch {
	case r.Refusal != "":
		return line + " — left alone: " + r.Refusal
	case r.Removed:
		return "removed " + line
	}
	return line
}

func (r Removal) relation() string {
	if r.Twin.SameBytes {
		return "is the same bytes as"
	}
	return "is a smaller file of the same name and second as"
}

// Cleanup is what one pass of the duplicates command found and did.
type Cleanup struct {
	Deleting bool
	Removals []Removal
	Removed  int
	Freed    int64
	Refused  int
}

func (c Cleanup) String() string {
	twins := len(c.Removals) - c.Refused
	bytes := int64(0)
	for _, removal := range c.Removals {
		if removal.Refusal == "" {
			bytes += removal.Twin.WrittenOff.SizeBytes
		}
	}
	var parts []string
	switch {
	case c.Removed > 0:
		parts = append(parts, fmt.Sprintf("%d written-off files removed, %s freed", c.Removed, humanBytes(c.Freed)))
	case twins == 0 && c.Refused == 0:
		parts = append(parts, "no written-off file is a copy of a photo still backed up")
	case c.Deleting:
		parts = append(parts, "nothing was removed")
	default:
		parts = append(parts, fmt.Sprintf("%d written-off files are copies of photos still backed up, %s; --delete removes them",
			twins, humanBytes(bytes)))
	}
	if c.Refused > 0 {
		parts = append(parts, fmt.Sprintf("%d left alone because the kept copy could not be verified", c.Refused))
	}
	return strings.Join(parts, ", ")
}

// RemoveTwins finds every written-off file whose photograph is still backed up under another
// key and, with remove set, deletes it. It is the one thing in gpb that deletes a file, and
// the rule under which it does is narrow: the copy that stays has to be there and hash to
// what was recorded for it, re-read now rather than trusted from the last run. A copy that
// cannot be read, or reads as something else, leaves both files where they are — the
// written-off one may be the only good copy left.
//
// Each twin is handled on its own: verify, unlink, then forget the row, so a crash leaves at
// most one file gone with its row still present, which the next pass finishes.
func RemoveTwins(ctx context.Context, db *store.Store, remove bool) (Cleanup, error) {
	twins, err := db.WrittenOffTwins()
	if err != nil {
		return Cleanup{}, err
	}
	if remove {
		log.Printf("duplicates: %d written-off files have a backed-up copy; removing them", len(twins))
	}

	cleanup := Cleanup{Deleting: remove}
	verified := map[string]string{}
	for _, twin := range twins {
		if err := ctx.Err(); err != nil {
			return cleanup, err
		}

		removal := Removal{Twin: twin}
		refusal, seen := verified[twin.Kept.MediaKey]
		if !seen {
			refusal = keptCopyIsIntact(twin.Kept)
			verified[twin.Kept.MediaKey] = refusal
		}
		removal.Refusal = refusal

		if removal.Refusal == "" && remove {
			if err := forgetTwin(db, twin); err != nil {
				return cleanup, err
			}
			removal.Removed = true
			cleanup.Removed++
			cleanup.Freed += twin.WrittenOff.SizeBytes
		}
		if removal.Refusal != "" {
			cleanup.Refused++
		}
		cleanup.Removals = append(cleanup.Removals, removal)
	}
	return cleanup, nil
}

// keptCopyIsIntact returns why the kept copy cannot be relied on, or nothing when it can.
func keptCopyIsIntact(kept store.MediaItem) string {
	if kept.SHA256 == "" {
		return "the kept copy has no recorded hash to check against"
	}
	digest, err := hashFile(kept.LocalPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "nothing is at the kept copy's path"
	case err != nil:
		return "the kept copy could not be read: " + err.Error()
	case !strings.EqualFold(digest, kept.SHA256):
		return fmt.Sprintf("the kept copy no longer hashes to what was recorded (recorded %s, found %s)",
			shortHash(kept.SHA256), shortHash(digest))
	}
	return ""
}

// forgetTwin unlinks the written-off file and then its row. A file already gone is a pass that
// stopped between the two, and finishes here.
func forgetTwin(db *store.Store, twin store.Twin) error {
	if err := os.Remove(twin.WrittenOff.LocalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing a written-off copy: %w", err)
	}
	return db.Forget(twin.WrittenOff.MediaKey)
}

// humanBytes uses power-of-ten units, as the web pages do, so a size printed here can be
// compared with one shown there.
func humanBytes(bytes int64) string {
	if bytes < 1000 {
		return fmt.Sprintf("%d B", bytes)
	}
	value, exponent := float64(bytes), 0
	for value >= 1000 && exponent < len(byteUnits)-1 {
		value /= 1000
		exponent++
	}
	return fmt.Sprintf("%.1f %cB", value, byteUnits[exponent])
}

const byteUnits = " kMGT"
