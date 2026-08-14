package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"gpb/internal/store"
)

// Fault is what verification found wrong with one backed-up file.
type Fault string

const (
	// FaultMissing is the store saying done over a path with nothing at it. A sync run reclaims
	// these at the start of every pass, so a sweep normally finds none — one here means the loss
	// happened since the last run, or to an item no run visits any more.
	FaultMissing Fault = "missing"

	// FaultCorrupt is the one only this sweep can find: the file is where it should be, the right
	// size to look untouched, and its bytes are no longer the bytes that were downloaded.
	FaultCorrupt Fault = "corrupt"

	// FaultUnreadable is neither of those — a permissions error, a disk that answered with an I/O
	// failure, an unmounted volume. It is reported and never repaired, because re-downloading a
	// library on the strength of a mount that came up late is a worse outcome than the fault.
	FaultUnreadable Fault = "unreadable"
)

type Problem struct {
	Item   store.MediaItem
	Fault  Fault
	Detail string
}

func (p Problem) String() string {
	return fmt.Sprintf("%s  %s  %s (%s)", p.Fault, p.Item.MediaKey, p.Item.LocalPath, p.Detail)
}

type Verification struct {
	Checked  int
	Intact   int
	Unhashed int
	Requeued int
	Problems []Problem
}

func (v Verification) String() string {
	parts := []string{fmt.Sprintf("%d files checked, %d intact", v.Checked, v.Intact)}
	if v.Unhashed > 0 {
		parts = append(parts, fmt.Sprintf("%d with no recorded hash", v.Unhashed))
	}
	for _, fault := range []Fault{FaultCorrupt, FaultMissing, FaultUnreadable} {
		if found := v.count(fault); found > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", found, fault))
		}
	}
	if v.Requeued > 0 {
		parts = append(parts, fmt.Sprintf("%d queued to be fetched again", v.Requeued))
	}
	return strings.Join(parts, ", ")
}

func (v *Verification) note(item store.MediaItem, fault Fault, detail string) {
	v.Problems = append(v.Problems, Problem{Item: item, Fault: fault, Detail: detail})
}

func (v Verification) count(fault Fault) int {
	found := 0
	for _, problem := range v.Problems {
		if problem.Fault == fault {
			found++
		}
	}
	return found
}

// progressInterval is how often a sweep says where it has got to. A library of a hundred
// thousand files is an hour of reading, and an hour of silence is indistinguishable from a hang.
const progressInterval = 2000

// Verify re-reads every file the store calls done and checks it still hashes to what was
// recorded when it arrived. That is a different question from the one a sync run asks: a run
// notices a file that has gone, but a file quietly rotted by a failing disk still passes every
// check gpb makes, because the only evidence is the hash and nothing reads it back.
//
// With repair set, a missing or corrupt file goes back on the work list and the next run fetches
// it again, renaming the good copy over the bad one. Nothing is deleted here even so: a corrupt
// file is still the only copy of that photograph until its replacement has landed, and an item
// whose album is no longer followed will not be fetched again at all — the report says it is
// wrong, and following the album again is what fixes it.
func Verify(ctx context.Context, db *store.Store, repair bool) (Verification, error) {
	items, err := db.DownloadedFiles()
	if err != nil {
		return Verification{}, err
	}
	log.Printf("verify: re-reading %d backed-up files", len(items))

	var report Verification
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return report, err
		}

		report.Checked++

		digest, err := hashFile(item.LocalPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
			report.note(item, FaultMissing, "nothing at that path")
		case err != nil:
			report.note(item, FaultUnreadable, err.Error())
		case item.SHA256 == "":
			report.Unhashed++
		case !strings.EqualFold(digest, item.SHA256):
			report.note(item, FaultCorrupt,
				fmt.Sprintf("recorded %s, found %s", shortHash(item.SHA256), shortHash(digest)))
		default:
			report.Intact++
		}

		if report.Checked%progressInterval == 0 {
			log.Printf("verify: %d of %d files read, %d intact", report.Checked, len(items), report.Intact)
		}
	}

	if repair {
		requeued, err := requeueFaulty(db, report.Problems)
		report.Requeued = requeued
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

func requeueFaulty(db *store.Store, problems []Problem) (int, error) {
	requeued := 0
	for _, problem := range problems {
		if problem.Fault == FaultUnreadable {
			continue
		}
		if err := db.SetItemState(problem.Item.MediaKey, store.StateDiscovered); err != nil {
			return requeued, err
		}
		requeued++
	}
	if requeued > 0 {
		log.Printf("verify: %d files will be fetched again", requeued)
	}
	return requeued, nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func shortHash(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12] + "…"
}
