package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const WorkflowSpecMismatchPendingMessage = "Workflow spec mismatch; cleanup pending"
const OberthStopReasonPrefix = "cancelling by oberth: "

const ExecutorIdentityPendingMessage = "Executor identity readiness deadline exceeded after one issuance retry; cleanup pending"

// WorkflowCleanupReason admits only server-authored fixed reasons. Transport
// errors and untrusted controller messages cannot become durable public text.
func WorkflowCleanupReason(reason string) bool {
	return reason == WorkflowSpecMismatchPendingMessage || reason == ExecutorIdentityPendingMessage || reason == argoworkflow.ErrWorkflowCapabilityRequired.Error()
}

// MarkRunCreatePending retains an uncertain submission and re-arms its cleanup
// atomically, even if cancel already completed an earlier NotFound.
// An optional fixed reason survives restart and subsequent re-arms.
// owner_restart also denotes a lost submission observer; no schema change.
func (s *Store) MarkRunCreatePending(ctx context.Context, id, workflowName string, reason ...string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(workflowName) == "" {
		return fmt.Errorf("%w: run ID and Job name required", ErrInvalid)
	}
	if len(reason) > 1 || (len(reason) == 1 && reason[0] != "" && !WorkflowCleanupReason(reason[0])) {
		return fmt.Errorf("%w: unsupported Workflow cleanup reason", ErrInvalid)
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
	if run.WorkflowName != workflowName || run.Status != model.RunRunning {
		return fmt.Errorf("%w: run cannot own pending create", ErrInvalidState)
	}
	now := unixNano(s.now())
	message := "Workflow create outcome uncertain; cleanup pending"
	if WorkflowCleanupReason(run.Error) {
		message = run.Error
	}
	if len(reason) == 1 && WorkflowCleanupReason(reason[0]) {
		message = reason[0]
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET phase=CASE WHEN status='running' AND phase!='cancelled' THEN 'infrastructure-pending' ELSE phase END, error=?, reason=CASE WHEN phase='cancelled' THEN reason ELSE ? END, updated_at=? WHERE id=?`, message, OberthStopReasonPrefix+strings.TrimSuffix(message, "; cleanup pending"), now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO run_cancellations(run_id, job_name, superseded_by, reason, created_at)
VALUES(?, ?, ?, 'owner_restart', ?)
ON CONFLICT(run_id) DO UPDATE SET job_name=excluded.job_name, reason='owner_restart', created_at=excluded.created_at, completed_at=NULL`, id, workflowName, "", now); err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteObservedRunCancellation cannot retire an obligation that was re-armed
// after this pass read it (for example, after cleanup observed NotFound while a
// timed-out POST was still in flight).
func (s *Store) CompleteObservedRunCancellation(ctx context.Context, observed model.RunCancellation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE run_cancellations SET completed_at=?
 WHERE run_id=? AND job_name=? AND reason=? AND created_at=? AND completed_at IS NULL`, unixNano(s.now()), observed.RunID, observed.WorkflowName, observed.Reason, unixNano(observed.CreatedAt))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 1 {
		// A cancelled run keeps its classification, but must not keep
		// displaying "cleanup pending" after the exact obligation is completed.
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET error=reason, updated_at=? WHERE id=? AND status='interrupted' AND error IN ('Workflow create outcome uncertain; cleanup pending', ?, ?)`, unixNano(s.now()), observed.RunID, WorkflowSpecMismatchPendingMessage, ExecutorIdentityPendingMessage); err != nil {
			return err
		}
		return tx.Commit()
	}
	var completed sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT completed_at FROM run_cancellations WHERE run_id=? AND job_name=? AND reason=? AND created_at=?`, observed.RunID, observed.WorkflowName, observed.Reason, unixNano(observed.CreatedAt)).Scan(&completed)
	if err == nil && completed.Valid {
		return tx.Commit()
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return fmt.Errorf("%w: cancellation changed after observation", ErrInvalidState)
}
