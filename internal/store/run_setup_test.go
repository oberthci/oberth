package store

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// Historical fixtures select a row explicitly before testing later transitions.
func (s *Store) ClaimNextRun(ctx context.Context) (model.Run, error) {
	return s.claimNextRun(ctx, "")
}

func (s *Store) claimNextRun(ctx context.Context, id string) (model.Run, error) {
	// An explicit transaction ensures the candidate is known before commit.
	// On a lost commit acknowledgement the scheduler can resolve that exact
	// run instead of claiming a second run and abandoning the first.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := unixNano(s.now())
	value, err := scanRun(tx.QueryRowContext(ctx, `
UPDATE runs
SET status = 'running', phase = 'waiting', updated_at = ?
WHERE queue_sequence = (
    SELECT queue_sequence FROM runs WHERE (?='' OR id=?) AND `+claimableRunSQL+`
    ORDER BY `+submissionOrderSQL()+` LIMIT 1
)
RETURNING `+runColumns, now, id, id))
	if err != nil {
		return model.Run{}, translateNotFound("queued run", err)
	}
	if err := tx.Commit(); err != nil {
		return value, fmt.Errorf("commit run claim: %w", err)
	}
	return value, nil
}

func submissionOrderSQL() string { return "admission_sequence, queue_sequence" }

const claimableRunSQL = `status='queued' AND NOT EXISTS (SELECT 1 FROM run_cancellations WHERE run_cancellations.run_id=runs.id AND completed_at IS NULL) AND NOT EXISTS (SELECT 1 FROM publications p WHERE p.status='pending' AND p.repo_id=runs.repo_id AND p.ref_kind=runs.ref_kind AND p.ref=runs.ref)`

func (s *Store) NextQueuedRun(ctx context.Context) (model.Run, error) {
	return scanRun(s.reader().QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE `+claimableRunSQL+` ORDER BY `+submissionOrderSQL()+` LIMIT 1`))
}
