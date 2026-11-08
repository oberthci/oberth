package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

// These server-owned reasons preserve lineage without a schema migration.
// Manual reruns append the operator's reason after a server-owned lineage
// header. Only that header can determine the inherited automatic budget.
const legacyAutoRetryOfPrefix = "automatic infrastructure retry of "
const autoRetryOfPrefix = "automatic retry of "
const autoRetryCauseSeparator = " after infrastructure failure: "
const autoRetriedByPrefix = "infra error, retried as "
const manualAutoRetryLineage = " (lineage: automatic retry of "

// automaticRetrySuccessor is shared by row scans and display-status filtering.
func automaticRetrySuccessor(reason string) string {
	lineage := strings.TrimSuffix(reason, "; "+PolicyRefreshWaitingReason)
	successor := strings.TrimPrefix(lineage, autoRetriedByPrefix)
	if successor == lineage {
		return ""
	}
	return successor
}

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
	var cause string
	if err := tx.QueryRowContext(ctx, `SELECT error FROM runs WHERE id=?`, origin).Scan(&cause); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	next.AutoRetryOf, next.Reason = origin, autoRetryOfPrefix+origin+autoRetryCauseSeparator+cause
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
func (s *Store) autoRetryInfrastructureTx(ctx context.Context, tx *sql.Tx, source model.Run, observation *model.RunTerminalObservation, now int64) (model.Run, error) {
	if !model.IsInfrastructureRunFailure(source.Status, source.Phase, source.FailedStep) || pushretry.PublicationFailure(source.Error) || source.FailedStage != "" || source.RefKind != model.RefBranch || source.Release || source.AutoRetryOf != "" || source.AutoRetriedBy != "" || source.SupersededBy != "" || (source.Trigger != "branch" && source.Trigger != "push" && source.Trigger != "schedule" && source.Trigger != "manual") {
		return source, nil
	}
	eviction := source.Phase == "infrastructure" && observation.EvictionOnly() && observation.WorkflowName == source.WorkflowName
	if eviction {
		steps, err := stepResultsFrom(ctx, tx, source.ID)
		if err != nil {
			return source, err
		}
		for _, step := range steps {
			if step.Status == model.StepFailed && !observation.EvictedAttempt(step.Stage, step.Step, step.StartedAt, step.FinishedAt) {
				return source, nil
			}
		}
	}
	deadline := model.IsExecutionDeadlineReason(source.Error)
	if observation != nil {
		for _, step := range observation.Steps {
			if (step.Status == model.StepFailed && (!eviction || !step.Infrastructure())) || (deadline && (step.Started || step.Status == model.StepRunning || step.Status == model.StepPassed)) {
				return source, nil
			}
		}
	}
	if err := requireRepositoryActive(ctx, tx, source.RepoID); err != nil {
		if errors.Is(err, ErrRepositoryArchived) {
			return source, nil
		}
		return source, err
	}
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT
  (? OR (
    NOT EXISTS(SELECT 1 FROM runs WHERE repo_id=? AND ref_kind=? AND ref=? AND schedule_entry=? AND queue_sequence>?)
    AND NOT EXISTS(SELECT 1 FROM runs WHERE repo_id=? AND ref_kind=? AND ref=? AND schedule_entry=? AND status IN ('queued','running'))
  ))
  AND NOT EXISTS(SELECT 1 FROM step_results WHERE run_id=?
    AND ((status='failed' AND NOT ?) OR (? AND (started_at IS NOT NULL OR status='passed' OR log_end>log_start))))
  AND NOT EXISTS(SELECT 1 FROM publications WHERE run_id=?)
  AND NOT EXISTS(SELECT 1 FROM run_cancellations WHERE run_id=? AND completed_at IS NULL)`, source.PriorityProtected, source.RepoID, source.RefKind, source.Ref, source.ScheduleEntry, source.QueueSequence, source.RepoID, source.RefKind, source.Ref, source.ScheduleEntry, source.ID, eviction, deadline, source.ID, source.ID).Scan(&eligible)
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
	// Every automatic retry retains its source's admission position. The
	// inherited sequence fences supersession away from all other queued work.
	inheritedSequence := source.AdmissionSequence
	next, err := s.enqueueRunTx(ctx, tx, model.RunSpec{RepoID: source.RepoID, RefKind: source.RefKind, Ref: source.Ref, SHA: source.SHA, Actor: source.Actor, Trigger: source.Trigger, ScheduleEntry: source.ScheduleEntry, TestedSHA: source.TestedSHA, BaseSHA: source.BaseSHA}, id, now, inheritedSequence)
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
	details, err := json.Marshal(map[string]any{"source_run_id": source.ID, "run_id": next.ID, "reason": next.Reason, "failure_reason": source.Error, "admission_sequence": source.AdmissionSequence, "priority": next.Priority, "automatic": true, "attempt": 1, "sha": source.SHA, "ref": source.Ref})
	if err != nil {
		return source, err
	}
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: "oberth", Action: "run.retry.auto", ResourceType: "run", ResourceID: next.ID, Details: string(details)}, now); err != nil {
		return source, err
	}
	return source, nil
}

// Keep old receipts readable while the human reason now includes the cause.
func automaticRetryOrigin(reason string) string {
	reason = strings.TrimPrefix(strings.TrimSuffix(reason, "; "+PolicyRefreshWaitingReason), RestartRequeueReason+"; ")
	if rest, ok := strings.CutPrefix(reason, "rerun of "); ok {
		source, lineage, found := strings.Cut(rest, manualAutoRetryLineage)
		origin, suffix, closed := strings.Cut(lineage, ")")
		if found && closed && len(source) == 32 && len(origin) == 32 &&
			strings.Trim(source+origin, "0123456789abcdef") == "" && (suffix == "" || strings.HasPrefix(suffix, ": ")) {
			return origin
		}
		return ""
	}
	for _, prefix := range []string{autoRetryOfPrefix, legacyAutoRetryOfPrefix} {
		if rest, ok := strings.CutPrefix(reason, prefix); ok {
			id, _, _ := strings.Cut(rest, autoRetryCauseSeparator)
			return id
		}
	}
	return ""
}
