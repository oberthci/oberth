package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	// RunTimingRetainedRuns is how many of the newest terminal runs keep their
	// Workflow and step timing records. Older detail is the bulk of the per-run
	// rows (a Workflow record holds up to 256 KiB, each step up to 16 KiB),
	// and the backup ceiling is fixed, so it is pruned in bounded passes.
	RunTimingRetainedRuns = 2000
	runTimingSweepRuns    = 200
	runTimingDeleteBatch  = 20
	runTimingPassBudget   = 2 * time.Second
)

// PruneRunTiming removes the Workflow and step timing records of terminal runs
// older than the newest keepRuns terminal runs. Runs the evidence retention
// protects (active, release and tag, pending publication or promotion, open
// issue on the ref, the newest keepNewest per ref) keep their records. The run
// row, its step results and admission episodes stay; the run's milestones stay
// and its timing reads unavailable=pruned. One pass handles at most 200 runs,
// 20 per transaction, within two seconds; the next sweep continues.
func (s *Store) PruneRunTiming(ctx context.Context, keepRuns, keepNewest int) (int64, error) {
	if s.readOnly {
		return 0, ErrInvalidState
	}
	if keepRuns < 1 || keepNewest < 0 {
		return 0, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, runTimingPassBudget)
	defer cancel()
	var cutoff int64
	err := s.reader().QueryRowContext(ctx, `SELECT queue_sequence FROM runs WHERE finished_at IS NOT NULL ORDER BY queue_sequence DESC LIMIT 1 OFFSET ?`, keepRuns-1).Scan(&cutoff)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := s.reader().QueryContext(ctx, `SELECT r.id FROM runs r
WHERE r.finished_at IS NOT NULL AND r.queue_sequence < ?
 AND (EXISTS(SELECT 1 FROM run_timings t WHERE t.run_id=r.id AND t.workflow_json IS NOT NULL)
  OR EXISTS(SELECT 1 FROM step_timings t WHERE t.run_id=r.id)
  OR EXISTS(SELECT 1 FROM restart_workflow_timings t WHERE t.run_id=r.id)
  OR EXISTS(SELECT 1 FROM restart_step_timings t WHERE t.run_id=r.id))
 AND NOT (`+evidenceProtectionSQL+`)
ORDER BY r.queue_sequence LIMIT ?`, cutoff, unixNano(s.now()), keepNewest, keepNewest, runTimingSweepRuns)
	if err != nil {
		return 0, err
	}
	selected, err := scanRunTimingCandidates(rows)
	if err != nil {
		return 0, err
	}
	var pruned int64
	for len(selected) > 0 {
		batch := selected[:min(len(selected), runTimingDeleteBatch)]
		n, err := s.pruneRunTimingBatch(ctx, batch, keepNewest)
		pruned += n
		if err != nil {
			return pruned, err
		}
		selected = selected[len(batch):]
	}
	return pruned, nil
}

// scanRunTimingCandidates reads the selection and closes it before the
// prune batches take the writer.
func scanRunTimingCandidates(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var selected []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		selected = append(selected, id)
	}
	return selected, rows.Err()
}

func (s *Store) pruneRunTimingBatch(ctx context.Context, ids []string, keepNewest int) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var pruned int64
	for _, id := range ids {
		// Recheck inside the write transaction: a selected run can have become
		// protected (a new issue, a pending promotion) since the read.
		var eligible bool
		err := tx.QueryRowContext(ctx, `SELECT r.finished_at IS NOT NULL AND NOT (`+evidenceProtectionSQL+`) FROM runs r WHERE r.id=?`, unixNano(s.now()), keepNewest, keepNewest, id).Scan(&eligible)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !eligible) {
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, statement := range []string{
			`DELETE FROM step_timings WHERE run_id=?`,
			`DELETE FROM restart_step_timings WHERE run_id=?`,
			`DELETE FROM restart_workflow_timings WHERE run_id=?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, id); err != nil {
				return 0, err
			}
		}
		// run_timings is immutable once a value is set; replace the row and keep
		// every milestone. A kept workflow_created_at without workflow_json is the
		// pruned marker RunTiming reads.
		var claimed, checkout, seed, source, created, terminal sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT claimed_at,checkout_ready_at,source_seed_started_at,source_ready_at,workflow_created_at,terminal_at FROM run_timings WHERE run_id=? AND workflow_json IS NOT NULL`, id).Scan(&claimed, &checkout, &seed, &source, &created, &terminal)
		if err == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM run_timings WHERE run_id=?`, id); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO run_timings(run_id,version,claimed_at,checkout_ready_at,source_seed_started_at,source_ready_at,workflow_created_at,terminal_at) VALUES(?,1,?,?,?,?,?,?)`, id, claimed, checkout, seed, source, created, terminal); err != nil {
				return 0, err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		pruned++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("prune run timing: %w", err)
	}
	return pruned, nil
}

// DatabaseBytes is the size of the live database file and its write-ahead log,
// the figure the backup size ceiling is measured against. Zero when unknown.
func (s *Store) DatabaseBytes() int64 {
	if s.livePath == "" {
		return 0
	}
	var total int64
	for _, path := range []string{s.livePath, s.livePath + "-wal"} {
		if info, err := os.Stat(path); err == nil {
			total += info.Size()
		}
	}
	return total
}
