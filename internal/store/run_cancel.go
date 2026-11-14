package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/oberthci/oberth/internal/model"
)

// ErrRunNotActive marks a cancel of a run that is already terminal. It wraps
// ErrInvalidState so callers that only distinguish the class keep working.
var ErrRunNotActive = fmt.Errorf("%w: run is not active", ErrInvalidState)

// CancelRun records an audited Stop request. Claimed work remains active until
// Argo completes; queued work cannot own a Workflow and finishes immediately.
func (s *Store) CancelRun(ctx context.Context, request model.RunCancelRequest) (model.RunCancelResult, error) {
	request.Reason = strings.TrimSpace(request.Reason)
	if strings.TrimSpace(request.RunID) == "" || strings.TrimSpace(request.Actor) == "" {
		return model.RunCancelResult{}, fmt.Errorf("%w: run ID and actor are required", ErrInvalid)
	}
	if err := ValidateCancelReason(request.Reason); err != nil {
		return model.RunCancelResult{}, err
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.RunCancelResult{}, fmt.Errorf("begin run cancel: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, request.RunID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.RunCancelResult{}, fmt.Errorf("%w: run", ErrNotFound)
	}
	if err != nil {
		return model.RunCancelResult{}, fmt.Errorf("load run for cancel: %w", err)
	}
	if current.Cancelled() || current.Status.Active() {
		if request.Authorize != nil {
			warning, err := request.Authorize(current)
			if err != nil {
				return model.RunCancelResult{}, err
			}
			request.Warning = warning
		}
	}
	if current.Cancelled() {
		result := model.RunCancelResult{Run: current, PreviousState: current.Status}
		if !current.Status.Active() {
			return result, tx.Commit()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_cancellations(run_id,job_name,reason,created_at,terminate) VALUES(?,?,'cancelled',?,1)
ON CONFLICT(run_id) DO UPDATE SET terminate=1,created_at=excluded.created_at,completed_at=NULL`, current.ID, current.WorkflowName, now); err != nil {
			return result, err
		}
		details, _ := json.Marshal(map[string]any{"run_id": current.ID, "shutdown": "Terminate", "escalated": true, "reason": request.Reason})
		if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: request.Actor, Action: "run.cancel", ResourceType: "run", ResourceID: current.ID, Details: string(details)}, now); err != nil {
			return result, err
		}
		pending, err := scanRunCancellation(tx.QueryRowContext(ctx, `SELECT run_id,job_name,reason,created_at,completed_at,terminate FROM run_cancellations WHERE run_id=?`, current.ID))
		if err != nil {
			return result, err
		}
		result.Cancellations = []model.RunCancellation{pending}
		return result, tx.Commit()
	}
	if !current.Status.Active() {
		state := string(current.Status)
		return model.RunCancelResult{}, fmt.Errorf("%w: run %s is already %s; nothing to cancel", ErrRunNotActive, current.ID, state)
	}
	var pendingPublications int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM publications WHERE run_id = ? AND status = 'pending'`, current.ID).Scan(&pendingPublications); err != nil {
		return model.RunCancelResult{}, fmt.Errorf("check run publications before cancel: %w", err)
	}
	if pendingPublications > 0 {
		return model.RunCancelResult{}, fmt.Errorf("%w: run %s passed and owns a pending upstream publication; it can no longer be cancelled", ErrInvalidState, current.ID)
	}
	previous := current.Status
	if current.Status == model.RunRunning && current.StartedAt == nil {
		previous = model.RunWaiting
	}
	text := "cancelling by " + request.Actor
	if current.Status == model.RunQueued {
		text = "cancelled by " + request.Actor
	}
	if request.Reason != "" {
		text += ": " + request.Reason
	}
	cancelled, err := scanRun(tx.QueryRowContext(ctx, `
UPDATE runs
SET status = CASE WHEN status='queued' THEN 'interrupted' ELSE status END, phase = ?, reason = ?, error = CASE WHEN status='queued' THEN ? ELSE error END, finished_at = CASE WHEN status='queued' THEN ? ELSE NULL END, updated_at = ?
WHERE id = ? AND status IN ('queued', 'running')
RETURNING `+runColumns, model.PhaseCancelled, text, text, now, now, current.ID))
	if err != nil {
		return model.RunCancelResult{}, fmt.Errorf("cancel run: %w", err)
	}
	result := model.RunCancelResult{Run: cancelled, PreviousState: previous}
	terminate := current.Release || current.RefKind == model.RefTag || current.Trigger == "promotion" || current.Trigger == "release" || current.Trigger == "apply"
	if current.Status == model.RunRunning {
		cleanupReason := "cancelled"
		if current.Phase == "infrastructure-pending" {
			cleanupReason = "owner_restart"
		}
		// A claimed owner retains a durable Stop obligation. An
		// empty Job name means the owner has not published one yet; it does
		// so through SetRunWorkflowName, or settles the obligation when it exits.
		if _, err := tx.ExecContext(ctx, `
INSERT INTO run_cancellations(run_id, job_name, reason, created_at, terminate)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(run_id) DO UPDATE SET
    job_name = excluded.job_name,
    reason = excluded.reason,
    created_at = excluded.created_at,
    completed_at = NULL, terminate=MAX(run_cancellations.terminate, excluded.terminate)`, cancelled.ID, cancelled.WorkflowName, cleanupReason, now, terminate); err != nil {
			return model.RunCancelResult{}, fmt.Errorf("record run cancellation: %w", err)
		}
		result.Cancellations = []model.RunCancellation{{
			RunID: cancelled.ID, WorkflowName: cancelled.WorkflowName, Reason: cleanupReason, CreatedAt: fromUnixNano(now), Terminate: terminate,
		}}
	}
	if cancelled.Trigger == "promotion" {
		promotion, err := promotionByRunTx(ctx, tx, cancelled.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, ErrNotFound) {
			return model.RunCancelResult{}, fmt.Errorf("load promotion for cancelled run: %w", err)
		}
		if err == nil && promotion.Status == model.PromotionPending && cancelled.Status.Terminal() {
			if _, err := finishPromotionTx(ctx, tx, promotion, model.PromotionFailed, cancelled.ID, "promotion CI "+text, now); err != nil {
				return model.RunCancelResult{}, err
			}
		}
		if err == nil {
			result.PromotionID = promotion.ID
		}
	}
	details, err := json.Marshal(map[string]any{
		"run_id": cancelled.ID, "repo_id": cancelled.RepoID, "ref_kind": cancelled.RefKind, "ref": cancelled.Ref,
		"sha": cancelled.SHA, "trigger": cancelled.Trigger, "previous_state": previous, "job_name": cancelled.WorkflowName,
		"reason": request.Reason, "force": request.Force, "shutdown": map[bool]string{false: "Stop", true: "Terminate"}[terminate],
		"warning": request.Warning, "promotion_id": result.PromotionID,
	})
	if err != nil {
		return model.RunCancelResult{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: request.Actor, Action: "run.cancel",
		ResourceType: "run", ResourceID: cancelled.ID, Details: string(details)}, now); err != nil {
		return model.RunCancelResult{}, fmt.Errorf("audit run cancel: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.RunCancelResult{}, fmt.Errorf("commit run cancel: %w", err)
	}
	return result, nil
}

// ValidateCancelReason bounds the free-text reason recorded with a cancel:
// at most model.MaximumCancelReasonBytes of printable text on one line.
func ValidateCancelReason(reason string) error {
	if len(reason) > model.MaximumCancelReasonBytes {
		return fmt.Errorf("%w: cancel reason exceeds %d bytes", ErrInvalid, model.MaximumCancelReasonBytes)
	}
	for _, character := range reason {
		if unicode.IsControl(character) || character == unicode.ReplacementChar {
			return fmt.Errorf("%w: cancel reason must be printable text on one line", ErrInvalid)
		}
	}
	return nil
}

// recoverCancellationObligationsTx retires only observations whose previous
// owner is gone; live or uncertain Workflows keep their Stop custody.
func recoverCancellationObligationsTx(ctx context.Context, tx *sql.Tx, now int64) error {
	// An earlier owner may have recorded the old unconditional restart
	// deletion. A resumable Workflow is now owned by reconciliation;
	// only explicit Stop obligations still authorize stopping it.
	if _, err := tx.ExecContext(ctx, `
UPDATE run_cancellations SET completed_at = ?
WHERE reason = 'owner_restart' AND completed_at IS NULL
  AND EXISTS (
      SELECT 1 FROM runs
      WHERE runs.id = run_cancellations.run_id AND status = 'running'
        AND phase NOT IN ('infrastructure-pending','cancelled')
        AND job_name != ''

  )`, now); err != nil {
		return fmt.Errorf("retire Workflow restart cancellation observations: %w", err)
	}
	// A cancel can win after ClaimNextRun but before the background task
	// publishes its deterministic Job name. The live background task may still be checking out in that
	// window, so ordinary startup recovery must leave the empty-name obligation
	// pending. Owner startup proves that background task is gone and can terminalize the
	// unsubmitted cancellation without an unbound Kubernetes mutation.
	if _, err := tx.ExecContext(ctx, `
UPDATE run_cancellations
SET completed_at = ?
WHERE completed_at IS NULL AND job_name = '' AND reason = 'cancelled'
  AND EXISTS (
      SELECT 1 FROM runs
      WHERE runs.id = run_cancellations.run_id
        AND runs.status = 'interrupted'
        AND runs.phase = 'cancelled'
  )`, now); err != nil {
		return fmt.Errorf("complete owner-restart no-Job cancellations: %w", err)
	}
	return nil
}
