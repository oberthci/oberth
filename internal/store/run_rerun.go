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

// RerunRun queues a new run with the same repository, ref, SHA, trigger,
// tested and base SHA as a failed or interrupted run (#1098), whatever step
// failed and whether or not it is the newest run of its ref. The checks, the
// enqueue and the run.rerun audit action commit together.
//
// Refused, without any change: an active, passed or publishing-failed source
// (a publishing failure is retried by publish_retry, which re-pushes without
// re-running anything); promotion, plan and apply runs, which promote and the
// trusted plan flow re-drive; a ref that already has an active run; and a tag
// whose tag object is already published, pending publication, or green. The
// rerun's own tag publication still classifies the remote first, so the tag
// object is pushed at most once.
func (s *Store) RerunRun(ctx context.Context, request model.RunRerunRequest) (model.RunRerunResult, error) {
	request.Reason = strings.TrimSpace(request.Reason)
	if strings.TrimSpace(request.RunID) == "" || strings.TrimSpace(request.Actor) == "" {
		return model.RunRerunResult{}, fmt.Errorf("%w: run ID and actor are required", ErrInvalid)
	}
	if err := ValidateCancelReason(request.Reason); err != nil {
		return model.RunRerunResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.RunRerunResult{}, fmt.Errorf("begin run rerun: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	source, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, request.RunID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.RunRerunResult{}, fmt.Errorf("%w: run", ErrNotFound)
	}
	if err != nil {
		return model.RunRerunResult{}, fmt.Errorf("load rerun source run: %w", err)
	}
	switch {
	case source.Status.Active():
		return model.RunRerunResult{}, fmt.Errorf("%w: run %s is still %s; wait for it or cancel it first", ErrInvalidState, source.ID, source.Status)
	case source.Status == model.RunPassed:
		return model.RunRerunResult{}, fmt.Errorf("%w: run %s already passed", ErrInvalidState, source.ID)
	case source.Status == model.RunFailed && source.Phase == "publishing":
		return model.RunRerunResult{}, fmt.Errorf("%w: run %s passed every step and only its upstream publication failed; publish_retry re-pushes it without re-running", ErrInvalidState, source.ID)
	case source.Trigger == "promotion":
		return model.RunRerunResult{}, fmt.Errorf("%w: run %s is promotion CI; retry its promotion with promote_retry", ErrInvalidState, source.ID)
	case source.Trigger == "plan" || source.Trigger == "apply":
		return model.RunRerunResult{}, fmt.Errorf("%w: %s runs are re-driven by the trusted plan flow, not rerun", ErrInvalidState, source.Trigger)
	}
	var warning string
	if request.Authorize != nil {
		if warning, err = request.Authorize(source); err != nil {
			return model.RunRerunResult{}, err
		}
	}
	var activeID string
	err = tx.QueryRowContext(ctx, `
SELECT id FROM runs WHERE repo_id = ? AND ref_kind = ? AND ref = ? AND status IN ('queued', 'running')
ORDER BY queue_sequence LIMIT 1`, source.RepoID, source.RefKind, source.Ref).Scan(&activeID)
	if err == nil {
		return model.RunRerunResult{}, fmt.Errorf("%w: %s %s already has active run %s", ErrInvalidState, source.RefKind, source.Ref, activeID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.RunRerunResult{}, fmt.Errorf("check active runs before rerun: %w", err)
	}
	if source.RefKind == model.RefTag {
		if err := requireUnpublishedTagTx(ctx, tx, source); err != nil {
			return model.RunRerunResult{}, err
		}
	}
	spec, err := validateRunSpec(model.RunSpec{
		RepoID: source.RepoID, RefKind: source.RefKind, Ref: source.Ref, SHA: source.SHA, Actor: request.Actor,
		Release: source.Release, Credentialed: source.Release && source.Credentialed, Trigger: source.Trigger,
		TestedSHA: source.TestedSHA, BaseSHA: source.BaseSHA,
	})
	if err != nil {
		return model.RunRerunResult{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.RunRerunResult{}, fmt.Errorf("generate rerun run ID: %w", err)
	}
	now := unixNano(s.now())
	enqueued, err := s.enqueueRunTx(ctx, tx, spec, id, now)
	if err != nil {
		return model.RunRerunResult{}, fmt.Errorf("enqueue rerun: %w", err)
	}
	details, err := json.Marshal(map[string]any{
		"source_run_id": source.ID, "run_id": enqueued.ID, "repo_id": source.RepoID,
		"ref_kind": source.RefKind, "ref": source.Ref, "sha": source.SHA, "trigger": source.Trigger,
		"reason": request.Reason, "force": request.Force, "warning": warning,
	})
	if err != nil {
		return model.RunRerunResult{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: request.Actor, Action: "run.rerun",
		ResourceType: "run", ResourceID: enqueued.ID, Details: string(details)}, now); err != nil {
		return model.RunRerunResult{}, fmt.Errorf("audit run rerun: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.RunRerunResult{}, fmt.Errorf("commit run rerun: %w", err)
	}
	return model.RunRerunResult{Enqueued: enqueued, Warning: warning}, nil
}

// requireUnpublishedTagTx refuses to rerun a tag whose release already
// published, is publishing, or passed: a rerun would repeat release work
// whose tag publication already happened.
func requireUnpublishedTagTx(ctx context.Context, tx *sql.Tx, source model.Run) error {
	var publicationID, status string
	err := tx.QueryRowContext(ctx, `
SELECT id, status FROM publications
WHERE repo_id = ? AND ref_kind = 'tag' AND ref = ? AND status IN ('pending', 'delivered')
ORDER BY sequence DESC LIMIT 1`, source.RepoID, source.Ref).Scan(&publicationID, &status)
	if err == nil {
		return fmt.Errorf("%w: tag %s already has a %s publication (%s); it is never published twice", ErrInvalidState, source.Ref, status, publicationID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check tag publications before rerun: %w", err)
	}
	var passedID string
	err = tx.QueryRowContext(ctx, `
SELECT id FROM runs WHERE repo_id = ? AND ref_kind = 'tag' AND ref = ? AND status = 'passed'
ORDER BY queue_sequence DESC LIMIT 1`, source.RepoID, source.Ref).Scan(&passedID)
	if err == nil {
		return fmt.Errorf("%w: tag %s already released by run %s", ErrInvalidState, source.Ref, passedID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check released tag runs before rerun: %w", err)
	}
	return nil
}

// RunRerunOf names the run that run ID re-ran, from its run.rerun audit
// action, or "" for a run that is not a rerun.
func (s *Store) RunRerunOf(ctx context.Context, runID string) (string, error) {
	if strings.TrimSpace(runID) == "" {
		return "", fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	var source string
	err := s.db.QueryRowContext(ctx, `
SELECT json_extract(details, '$.source_run_id') FROM audit_actions
WHERE action = 'run.rerun' AND resource_type = 'run' AND resource_id = ?
ORDER BY id DESC LIMIT 1`, runID).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read run rerun lineage: %w", err)
	}
	return source, nil
}
