package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// MarkRunCreatePending retains an uncertain submission and re-arms its cleanup
// atomically, even if cancel/supersede already completed an earlier NotFound.
// owner_restart also denotes a lost submission observer; no schema change.
func (s *Store) MarkRunCreatePending(ctx context.Context, id, jobName string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(jobName) == "" {
		return fmt.Errorf("%w: run ID and Job name required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: run", ErrNotFound)
	}
	if err != nil {
		return err
	}
	if run.JobName != jobName || (run.Status != model.RunRunning && !run.Preempted()) {
		return fmt.Errorf("%w: run cannot own pending create", ErrInvalidState)
	}
	now := unixNano(s.now())
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET phase=CASE WHEN status='running' THEN 'infrastructure-pending' ELSE phase END, error='Workflow create outcome uncertain; cleanup pending', updated_at=? WHERE id=?`, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO run_cancellations(run_id, job_name, superseded_by, reason, created_at)
VALUES(?, ?, ?, 'owner_restart', ?)
ON CONFLICT(run_id) DO UPDATE SET job_name=excluded.job_name, reason='owner_restart', created_at=excluded.created_at, completed_at=NULL`, id, jobName, run.SupersededBy, now); err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteObservedRunCancellation cannot retire an obligation that was re-armed
// after this pass read it (for example, after Delete observed NotFound while a
// timed-out POST was still in flight).
func (s *Store) CompleteObservedRunCancellation(ctx context.Context, observed model.RunCancellation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE run_cancellations SET completed_at=?
 WHERE run_id=? AND job_name=? AND reason=? AND created_at=? AND completed_at IS NULL`, unixNano(s.now()), observed.RunID, observed.JobName, observed.Reason, unixNano(observed.CreatedAt))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		// A cancelled/superseded run keeps its classification, but must not keep
		// displaying "cleanup pending" after the exact obligation is completed.
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET error=reason, updated_at=? WHERE id=? AND status='interrupted' AND error='Workflow create outcome uncertain; cleanup pending'`, unixNano(s.now()), observed.RunID); err != nil {
			return err
		}
		return tx.Commit()
	}
	var completed sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT completed_at FROM run_cancellations WHERE run_id=? AND job_name=? AND reason=? AND created_at=?`, observed.RunID, observed.JobName, observed.Reason, unixNano(observed.CreatedAt)).Scan(&completed)
	if err == nil && completed.Valid {
		return tx.Commit()
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return fmt.Errorf("%w: cancellation changed after observation", ErrInvalidState)
}
