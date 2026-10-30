package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// RetryRun creates a fresh branch or admitted tag run for an interrupted
// or infrastructure-failed attempt. The eligibility check, enqueue, and audit record commit
// together, so a refused retry cannot leave a queued run behind.
func (s *Store) RetryRun(ctx context.Context, request model.RunRetryRequest) (model.EnqueueRunResult, error) {
	if strings.TrimSpace(request.RunID) == "" || strings.TrimSpace(request.Actor) == "" {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: run ID and actor are required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("begin run retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	previous, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, request.RunID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: run", ErrNotFound)
	}
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("load retry source run: %w", err)
	}
	if err := requireRepositoryActive(ctx, tx, previous.RepoID); err != nil {
		return model.EnqueueRunResult{}, err
	}
	var newestID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE repo_id = ? AND ref_kind = ? AND ref = ? AND schedule_entry = ?
		ORDER BY queue_sequence DESC LIMIT 1`, previous.RepoID, previous.RefKind, previous.Ref, previous.ScheduleEntry).Scan(&newestID); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("check latest ref run: %w", err)
	}
	if newestID != previous.ID {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: retry source is not the newest run for its ref", ErrInvalidState)
	}
	eligibleStatus := previous.Status == model.RunInterrupted ||
		(previous.Status == model.RunFailed && (previous.Phase == "infrastructure" || previous.Phase == "job") && previous.FailedStep == "")
	if !eligibleStatus || (previous.RefKind != model.RefBranch && previous.RefKind != model.RefTag) ||
		previous.Trigger == "promotion" || previous.Trigger == "plan" || previous.Trigger == "apply" {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: retry requires an interrupted or infrastructure-failed run without a failed step", ErrInvalidState)
	}
	if previous.Release || previous.RefKind == model.RefTag {
		if request.Authorize == nil {
			return model.EnqueueRunResult{}, fmt.Errorf("%w: tag/release retry requires replay safety authorization; use run_rerun with force if needed", ErrInvalidState)
		}
		evidence, err := replayEvidenceTx(ctx, tx, previous.ID)
		if err != nil {
			return model.EnqueueRunResult{}, err
		}
		if err := request.Authorize(previous, evidence); err != nil {
			return model.EnqueueRunResult{}, err
		}
	}
	if previous.RefKind == model.RefTag {
		if err := requireUnpublishedTagTx(ctx, tx, previous); err != nil {
			return model.EnqueueRunResult{}, err
		}
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE repo_id = ? AND ref_kind = ? AND ref = ? AND schedule_entry = ?
		AND status IN ('queued', 'running')`, previous.RepoID, previous.RefKind, previous.Ref, previous.ScheduleEntry).Scan(&active); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("check active ref runs: %w", err)
	}
	if active != 0 {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: ref already has an active run", ErrInvalidState)
	}
	spec, err := validateRunSpec(model.RunSpec{RepoID: previous.RepoID, RefKind: previous.RefKind,
		Ref: previous.Ref, SHA: previous.SHA, Actor: request.Actor, Release: previous.Release, Credentialed: previous.Release && previous.Credentialed, Trigger: previous.Trigger, ScheduleEntry: previous.ScheduleEntry,
		TestedSHA: previous.TestedSHA, BaseSHA: previous.BaseSHA})
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("generate retry run ID: %w", err)
	}
	now := unixNano(s.now())
	enqueued, err := s.enqueueRunTx(ctx, tx, spec, id, now, 0)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("enqueue retry: %w", err)
	}
	details, err := json.Marshal(map[string]any{"source_run_id": previous.ID, "run_id": enqueued.ID,
		"repo_id": previous.RepoID, "ref_kind": previous.RefKind, "ref": previous.Ref, "sha": previous.SHA})
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: request.Actor, Action: "run.retry",
		ResourceType: "run", ResourceID: enqueued.ID, Details: string(details)}, now); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("audit run retry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("commit run retry: %w", err)
	}
	return enqueued, nil
}
