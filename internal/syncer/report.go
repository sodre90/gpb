package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gpb/internal/gphotos"
	"gpb/internal/store"
)

func (s *Syncer) finish(report Report, err error) (Report, error) {
	report.Err = err
	report.Outcome = outcomeOf(report, err)

	if storeErr := s.store.FinishRun(store.SyncRun{
		ID:         report.RunID,
		Outcome:    report.Outcome,
		Listed:     report.Listed,
		Downloaded: report.Downloaded,
		Failed:     report.Failed,
		Bytes:      report.Bytes,
		Error:      runDetail(report),
	}, time.Now()); storeErr != nil {
		log.Printf("syncer: could not record the run: %v", storeErr)
	}
	return report, err
}

// outcomeOf separates the failures that need a human from the ones that need a retry.
// OutcomeOf classifies a failure that happened before a run could report for itself — while
// taking the run lock, warming the profile or connecting to Google. Those are recorded as runs
// of their own, and they deserve the same reading of the same errors as a run that got further:
// above all, a cancelled setup is a shutdown rather than a fault.
func OutcomeOf(err error) store.Outcome {
	return outcomeOf(Report{}, err)
}

func outcomeOf(report Report, err error) store.Outcome {
	switch {
	case errors.Is(err, gphotos.ErrSessionRejected):
		return store.OutcomeAuthRequired
	case errors.Is(err, gphotos.ErrProtocolDrift):
		return store.OutcomeDrift
	// A cancelled context means this process was asked to stop, which is a shutdown and not a
	// fault. Recording it as an error would leave the daemon believing the day's backup had been
	// attempted and answered, when in truth it was cut off partway.
	case errors.Is(err, context.Canceled):
		return store.OutcomeInterrupted
	case err != nil:
		return store.OutcomeError
	// An album left unread is not a failure — nothing was lost and nothing was written off — but
	// it is not the whole job either, and a run reported as ok is one nobody looks at again.
	case report.Failed > 0 || len(report.SkippedAlbums) > 0 || len(report.UnlistedAlbums) > 0:
		return store.OutcomePartial
	default:
		return store.OutcomeOK
	}
}

// runDetail is what the run page shows underneath a run. A run that stepped over albums finished
// with no error at all and still has something the reader needs told, and sync_runs has one
// column to tell them in — so the failure and the albums it did not read share it, in that order.
func runDetail(report Report) string {
	details := make([]string, 0, 3)
	if report.Err != nil {
		details = append(details, report.Err.Error())
	}
	if len(report.SkippedAlbums) > 0 {
		details = append(details, listAlbums(report.SkippedAlbums,
			"could not be listed and was left as it was",
			"could not be listed and were left as they were"))
	}
	if len(report.UnlistedAlbums) > 0 {
		details = append(details, listAlbums(report.UnlistedAlbums,
			"is no longer listed by Google and was not walked — everything it held is kept, "+
				"and unfollowing it will clear this notice",
			"are no longer listed by Google and were not walked — everything they held is kept, "+
				"and unfollowing them will clear this notice"))
	}
	return strings.Join(details, "\n")
}

func listAlbums(albums []string, one, many string) string {
	if len(albums) == 1 {
		return "One album " + one + ": " + albums[0]
	}
	return fmt.Sprintf("%d albums %s: %s", len(albums), many, strings.Join(albums, "; "))
}
