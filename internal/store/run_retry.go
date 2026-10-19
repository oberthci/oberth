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

// RetryRun creates a fresh branch run for an interrupted or infrastructure
// failed attempt. The eligibility check, enqueue, and audit record commit
// together, so a refused retry cannot leave a queued run behind.
func (s *Store) RetryRun(ctx context.Context, runID, actor string) (model.EnqueueRunResult, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(actor) == "" {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: run ID and actor are required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("begin run retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	previous, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: run", ErrNotFound)
	}
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("load retry source run: %w", err)
	}
	eligibleStatus := previous.Status == model.RunInterrupted ||
		(previous.Status == model.RunFailed && previous.Phase == "infrastructure" && previous.FailedBurn == "" && previous.FailedStep == "")
	if !eligibleStatus || previous.RefKind != model.RefBranch || previous.Release ||
		previous.Trigger == "promotion" || previous.Trigger == "plan" || previous.Trigger == "apply" {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: retry requires an interrupted or infrastructure-failed branch run", ErrInvalidState)
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE repo_id = ? AND ref_kind = 'branch' AND ref = ?
		AND status IN ('queued', 'running')`, previous.RepoID, previous.Ref).Scan(&active); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("check active branch runs: %w", err)
	}
	if active != 0 {
		return model.EnqueueRunResult{}, fmt.Errorf("%w: branch already has an active run", ErrInvalidState)
	}
	spec, err := validateRunSpec(model.RunSpec{RepoID: previous.RepoID, RefKind: previous.RefKind,
		Ref: previous.Ref, SHA: previous.SHA, Actor: actor, Trigger: previous.Trigger,
		TestedSHA: previous.TestedSHA, BaseSHA: previous.BaseSHA})
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("generate retry run ID: %w", err)
	}
	now := unixNano(s.now())
	enqueued, err := s.enqueueRunTx(ctx, tx, spec, id, now)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("enqueue retry: %w", err)
	}
	details, err := json.Marshal(map[string]any{"source_run_id": previous.ID, "run_id": enqueued.ID,
		"repo_id": previous.RepoID, "ref": previous.Ref, "sha": previous.SHA})
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "run.retry",
		ResourceType: "run", ResourceID: enqueued.ID, Details: string(details)}, now); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("audit run retry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("commit run retry: %w", err)
	}
	return enqueued, nil
}
