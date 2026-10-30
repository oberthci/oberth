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

// CancelRun terminalizes one active run on an actor's explicit request, in the
// same transaction as its audit action. Authorization is the caller's.
//
// The run becomes interrupted with phase cancelled and a reason naming the
// actor; it is never re-fired, because restart requeue and supersede select
// only active runs. A claimed run (waiting or running) additionally receives
// the durable Workflow-deletion obligation a supersede records, with no
// successor: the scheduler's cancellation pass deletes its Workflow (an
// absent one included) exactly as for a superseded run, and its owner then
// settles without recording a result. A run that owns a pending publication is
// refused: the publication, not the run, owns that outcome. A cancelled
// promotion run fails its promotion, so nothing is merged or pushed. No CI
// issue is projected: a cancel is not a red build.
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
	if !current.Status.Active() {
		state := string(current.Status)
		if current.Cancelled() {
			state = "cancelled (" + current.Reason + ")"
		}
		return model.RunCancelResult{}, fmt.Errorf("%w: run %s is already %s; nothing to cancel", ErrRunNotActive, current.ID, state)
	}
	if request.Authorize != nil {
		warning, err := request.Authorize(current)
		if err != nil {
			return model.RunCancelResult{}, err
		}
		request.Warning = warning
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
	text := "cancelled by " + request.Actor
	if request.Reason != "" {
		text += ": " + request.Reason
	}
	cancelled, err := scanRun(tx.QueryRowContext(ctx, `
UPDATE runs
SET status = 'interrupted', phase = ?, reason = ?, error = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND status IN ('queued', 'running')
RETURNING `+runColumns, model.PhaseCancelled, text, text, now, now, current.ID))
	if err != nil {
		return model.RunCancelResult{}, fmt.Errorf("cancel run: %w", err)
	}
	result := model.RunCancelResult{Run: cancelled, PreviousState: previous}
	if current.Status == model.RunRunning {
		cleanupReason := "superseded"
		if current.Phase == "infrastructure-pending" {
			cleanupReason = "owner_restart"
		}
		// The same obligation a supersede records, without a successor. An
		// empty Job name means the owner has not published one yet; it does
		// so through SetRunJobName, or settles the obligation when it exits.
		if _, err := tx.ExecContext(ctx, `
INSERT INTO run_cancellations(run_id, job_name, superseded_by, reason, created_at)
VALUES(?, ?, '', ?, ?)
ON CONFLICT(run_id) DO UPDATE SET
    job_name = excluded.job_name,
    superseded_by = '',
    reason = excluded.reason,
    completed_at = NULL`, cancelled.ID, cancelled.JobName, cleanupReason, now); err != nil {
			return model.RunCancelResult{}, fmt.Errorf("record run cancellation: %w", err)
		}
		result.Cancellations = []model.RunCancellation{{
			RunID: cancelled.ID, JobName: cancelled.JobName, Reason: cleanupReason, CreatedAt: fromUnixNano(now),
		}}
	}
	if cancelled.Trigger == "promotion" {
		promotion, err := promotionByRunTx(ctx, tx, cancelled.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, ErrNotFound) {
			return model.RunCancelResult{}, fmt.Errorf("load promotion for cancelled run: %w", err)
		}
		if err == nil && promotion.Status == model.PromotionPending {
			if _, err := finishPromotionTx(ctx, tx, promotion, model.PromotionFailed, cancelled.ID, "promotion CI "+text, now); err != nil {
				return model.RunCancelResult{}, err
			}
			result.PromotionID = promotion.ID
		}
	}
	details, err := json.Marshal(map[string]any{
		"run_id": cancelled.ID, "repo_id": cancelled.RepoID, "ref_kind": cancelled.RefKind, "ref": cancelled.Ref,
		"sha": cancelled.SHA, "trigger": cancelled.Trigger, "previous_state": previous, "job_name": cancelled.JobName,
		"concurrency_group": cancelled.ConcurrencyGroup, "reason": request.Reason, "force": request.Force,
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

// RearmRunCancellation makes the deletion obligation for a preempted run's
// Workflow pending again. The run's owner calls it when it created the
// Workflow after a cancellation pass had already settled the obligation and
// its own deletion failed: the next pass (or restart) then retries it instead
// of leaving the Workflow orphaned.
func (s *Store) RearmRunCancellation(ctx context.Context, runID, jobName string) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(jobName) == "" {
		return fmt.Errorf("%w: run ID and job name are required", ErrInvalid)
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO run_cancellations(run_id, job_name, superseded_by, reason, created_at)
SELECT id, ?, superseded_by, 'superseded', ?
FROM runs
WHERE id = ? AND status = 'interrupted' AND (superseded_by != '' OR phase = 'cancelled')
ON CONFLICT(run_id) DO UPDATE SET job_name = excluded.job_name, completed_at = NULL`,
		jobName, unixNano(s.now()), runID); err != nil {
		return fmt.Errorf("re-arm run cancellation: %w", err)
	}
	return nil
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
