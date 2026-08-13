package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Outcome is how a sync run ended. auth_required and drift are separated from a generic
// error because they need different responses: one asks the user to log in, the other says
// Google changed the protocol and the decoders need attention. interrupted is separated
// because it is not a failure at all — the daemon was told to stop — and it is the one
// ending that should be picked straight back up rather than waited out.
type Outcome string

const (
	OutcomeOK           Outcome = "ok"
	OutcomePartial      Outcome = "partial"
	OutcomeAuthRequired Outcome = "auth_required"
	OutcomeDrift        Outcome = "drift"
	OutcomeInterrupted  Outcome = "interrupted"
	OutcomeError        Outcome = "error"
)

type SyncRun struct {
	ID         int64
	StartedAt  time.Time
	FinishedAt time.Time
	Outcome    Outcome
	Listed     int
	Downloaded int
	Failed     int
	Bytes      int64
	Error      string
}

func (s *Store) StartRun(at time.Time) (int64, error) {
	result, err := s.db.Exec(`INSERT INTO sync_runs (started_at) VALUES (?)`, formatTime(at))
	if err != nil {
		return 0, fmt.Errorf("starting a sync run: %w", err)
	}
	return result.LastInsertId()
}

// FinishRun closes out a run. A run left unfinished by a crash keeps a null finished_at,
// which is how the status page can tell "interrupted" from "still going".
func (s *Store) FinishRun(run SyncRun, at time.Time) error {
	result, err := s.db.Exec(`
		UPDATE sync_runs SET finished_at = ?, outcome = ?, listed = ?, downloaded = ?,
			failed = ?, bytes = ?, error = ?
		WHERE id = ?`,
		formatTime(at), string(run.Outcome), run.Listed, run.Downloaded, run.Failed,
		run.Bytes, nullEmpty(run.Error), run.ID)
	if err != nil {
		return fmt.Errorf("finishing the sync run: %w", err)
	}
	return requireOneRow(result, "sync run", fmt.Sprint(run.ID))
}

func (s *Store) RecentRuns(limit int) ([]SyncRun, error) {
	rows, err := s.db.Query(`
		SELECT id, started_at, finished_at, outcome, listed, downloaded, failed, bytes, error
		FROM sync_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing sync runs: %w", err)
	}
	defer rows.Close()

	var runs []SyncRun
	for rows.Next() {
		var run SyncRun
		var startedAt, finishedAt, outcome, runError sql.NullString
		if err := rows.Scan(&run.ID, &startedAt, &finishedAt, &outcome, &run.Listed,
			&run.Downloaded, &run.Failed, &run.Bytes, &runError); err != nil {
			return nil, fmt.Errorf("scanning a sync run: %w", err)
		}
		run.StartedAt = parseTime(startedAt)
		run.FinishedAt = parseTime(finishedAt)
		run.Outcome = Outcome(outcome.String)
		run.Error = runError.String
		runs = append(runs, run)
	}
	return runs, rows.Err()
}
