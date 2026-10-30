package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// These server-owned reasons preserve lineage without a schema migration.
// Client-supplied rerun reasons are audit-only and never set these columns.
const autoRetryOfPrefix = "automatic infrastructure retry of "
const autoRetriedByPrefix = "infra error, retried as "

func (s *Store) inheritAutoRetryTx(ctx context.Context, tx *sql.Tx, previous model.Run, next *model.Run) error {
	origin := previous.AutoRetryOf
	if origin == "" && previous.AutoRetriedBy != "" {
		origin = previous.ID
	}
	if origin == "" && previous.Status == model.RunInterrupted {
		// Cancellation/supersession may replace the human reason. The immutable
		// lineage receipt survives that overwrite, so replay cannot reset the cap.
		var saved sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT json_extract(details,'$.origin_run_id') FROM audit_actions WHERE action='run.auto-retry-lineage' AND resource_type='run' AND resource_id=? ORDER BY id DESC LIMIT 1`, previous.ID).Scan(&saved)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		origin = saved.String
	}
	if origin == "" {
		return nil
	}
	next.AutoRetryOf, next.Reason = origin, autoRetryOfPrefix+origin
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET reason=? WHERE id=?`, next.Reason, next.ID); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"origin_run_id": origin})
	if err != nil {
		return err
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: "oberth", Action: "run.auto-retry-lineage", ResourceType: "run", ResourceID: next.ID, Details: string(body)}, unixNano(next.CreatedAt))
	return err
}

// Failure, retry, lineage and audit commit together. This is never a second
// attempt at publication or tag-tier execution, and never supersedes newer work.
func (s *Store) autoRetryPreStepTx(ctx context.Context, tx *sql.Tx, source model.Run, now int64) (model.Run, error) {
	if source.Status != model.RunFailed || source.Phase != "infrastructure" || source.FailedStep != "" || source.FailedBurn != "" || source.RefKind != model.RefBranch || source.Release || source.AutoRetryOf != "" || source.AutoRetriedBy != "" || source.SupersededBy != "" || (source.Trigger != "branch" && source.Trigger != "push" && source.Trigger != "schedule" && source.Trigger != "manual") {
		return source, nil
	}
	if err := requireRepositoryActive(ctx, tx, source.RepoID); err != nil {
		if errors.Is(err, ErrRepositoryArchived) {
			return source, nil
		}
		return source, err
	}
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT
  NOT EXISTS(SELECT 1 FROM runs WHERE repo_id=? AND ref_kind=? AND ref=? AND schedule_entry=? AND queue_sequence>?)
  AND NOT EXISTS(SELECT 1 FROM step_results WHERE run_id=? AND (started_at IS NOT NULL OR finished_at IS NOT NULL OR status NOT IN ('pending','queued')))
  AND NOT EXISTS(SELECT 1 FROM runs WHERE repo_id=? AND ref_kind=? AND ref=? AND schedule_entry=? AND status IN ('queued','running'))
  AND NOT EXISTS(SELECT 1 FROM publications WHERE run_id=?)
  AND NOT EXISTS(SELECT 1 FROM run_cancellations WHERE run_id=? AND completed_at IS NULL)`, source.RepoID, source.RefKind, source.Ref, source.ScheduleEntry, source.QueueSequence, source.ID, source.RepoID, source.RefKind, source.Ref, source.ScheduleEntry, source.ID, source.ID).Scan(&eligible)
	if err != nil {
		return source, fmt.Errorf("check automatic infrastructure retry: %w", err)
	}
	if !eligible {
		return source, nil
	}
	id, err := randomID()
	if err != nil {
		return source, err
	}
	next, err := s.enqueueRunTx(ctx, tx, model.RunSpec{RepoID: source.RepoID, RefKind: source.RefKind, Ref: source.Ref, SHA: source.SHA, Actor: source.Actor, Trigger: source.Trigger, ScheduleEntry: source.ScheduleEntry, TestedSHA: source.TestedSHA, BaseSHA: source.BaseSHA}, id, now, 0)
	if err != nil {
		return source, fmt.Errorf("enqueue automatic infrastructure retry: %w", err)
	}
	next.Run, err = s.inheritQueuePriorityTx(ctx, tx, source, next.Run)
	if err != nil {
		return source, err
	}
	source.AutoRetriedBy, source.Reason = next.ID, autoRetriedByPrefix+next.ID
	if _, err = tx.ExecContext(ctx, `UPDATE runs SET reason=? WHERE id=?`, source.Reason, source.ID); err != nil {
		return source, err
	}
	if err = s.inheritAutoRetryTx(ctx, tx, source, &next.Run); err != nil {
		return source, err
	}
	details, err := json.Marshal(map[string]any{"source_run_id": source.ID, "run_id": next.ID, "reason": "infra error, retried", "automatic": true, "attempt": 1, "sha": source.SHA, "ref": source.Ref})
	if err != nil {
		return source, err
	}
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: "oberth", Action: "run.retry.auto", ResourceType: "run", ResourceID: next.ID, Details: string(details)}, now); err != nil {
		return source, err
	}
	return source, nil
}
