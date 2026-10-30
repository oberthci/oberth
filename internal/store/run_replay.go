package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

const maxReplayChain = 64

func replayAttemptTx(ctx context.Context, tx *sql.Tx, run model.Run) (model.RunReplayAttempt, error) {
	steps, err := stepResultsFrom(ctx, tx, run.ID)
	if err != nil {
		return model.RunReplayAttempt{}, err
	}
	result := model.RunReplayAttempt{Run: run, Steps: steps}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM publications WHERE run_id=?)`, run.ID).Scan(&result.PublicationStarted); err != nil {
		return result, err
	}
	if !run.Release && run.RefKind != model.RefTag {
		return result, nil
	}
	var body sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN length(details)<=1048576 THEN details END FROM audit_actions WHERE action='run.observed-terminal' AND actor='oberth' AND resource_type='run' AND resource_id=? ORDER BY id DESC LIMIT 1`, run.ID).Scan(&body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if body.Valid {
		var observation model.RunTerminalObservation
		if json.Unmarshal([]byte(body.String), &observation) == nil && observation.Version == 1 && observation.JobName == run.JobName && observation.Nodes >= 0 && observation.Nodes <= 4096 && len(observation.Steps) <= 4096 {
			valid := true
			for _, step := range observation.Steps {
				if step.Burn == "" || step.Step == "" || !step.Status.Terminal() {
					valid = false
					break
				}
			}
			if valid {
				result.Observation = &observation
			}
		}
	}
	return result, nil
}

func replayChainRunsTx(ctx context.Context, tx *sql.Tx, source model.Run) ([]model.Run, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE repo_id=? AND ref_kind=? AND ref=? ORDER BY queue_sequence DESC LIMIT ?`, source.RepoID, source.RefKind, source.Ref, maxReplayChain+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var runs []model.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func replayEvidenceTx(ctx context.Context, tx *sql.Tx, source model.Run) (model.RunReplayEvidence, error) {
	result := model.RunReplayEvidence{ChainComplete: true, Newest: true}
	runs := []model.Run{source}
	if source.RefKind == model.RefTag || source.Release {
		var err error
		runs, err = replayChainRunsTx(ctx, tx, source)
		if err != nil {
			return result, err
		}
		result.Newest = len(runs) > 0 && runs[0].ID == source.ID
		if len(runs) > maxReplayChain {
			result.ChainComplete = false
			runs = runs[:maxReplayChain]
		}
	}
	for _, run := range runs {
		attempt, err := replayAttemptTx(ctx, tx, run)
		if err != nil {
			return result, fmt.Errorf("read replay evidence: %w", err)
		}
		result.Chain = append(result.Chain, attempt)
		if run.ID == source.ID {
			result.Steps = attempt.Steps
			result.PublicationStarted = attempt.PublicationStarted
			result.Observation = attempt.Observation
		}
	}
	return result, nil
}

// A terminal DB status is not proof that its Workflow has stopped. This fence
// cannot be overridden with force, and covers every attempt of a release ref.
func requireReplayDeletionCompleteTx(ctx context.Context, tx *sql.Tx, source model.Run) error {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run_cancellations c JOIN runs r ON r.id=c.run_id WHERE c.completed_at IS NULL AND (r.id=? OR (? AND r.repo_id=? AND r.ref_kind=? AND r.ref=?)))`, source.ID, source.Release || source.RefKind == model.RefTag, source.RepoID, source.RefKind, source.Ref).Scan(&pending)
	if err != nil {
		return fmt.Errorf("check pending replay deletion: %w", err)
	}
	if pending {
		return fmt.Errorf("%w: Workflow deletion is pending; wait for confirmed cleanup before retry or rerun (force cannot bypass this)", ErrInvalidState)
	}
	return nil
}

func (s *Store) recordTerminalObservationTx(ctx context.Context, tx *sql.Tx, run model.Run, observation *model.RunTerminalObservation, now int64) error {
	if observation == nil || (!run.Release && run.RefKind != model.RefTag) {
		return nil
	}
	if observation.Version != 1 || observation.JobName == "" || observation.JobName != run.JobName || observation.Nodes < 0 || observation.Nodes > 4096 || len(observation.Steps) > observation.Nodes || run.Status == model.RunInterrupted {
		return fmt.Errorf("%w: invalid terminal observation", ErrInvalid)
	}
	for _, step := range observation.Steps {
		if step.Burn == "" || step.Step == "" || !step.Status.Terminal() {
			return fmt.Errorf("%w: incomplete terminal step observation", ErrInvalid)
		}
	}
	body, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	if len(body) > 1<<20 {
		return fmt.Errorf("%w: terminal observation too large", ErrInvalid)
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: "oberth", Action: "run.observed-terminal", ResourceType: "run", ResourceID: run.ID, Details: string(body)}, now)
	return err
}

func replayAuditDetails(release bool, audit *model.RunReplayAudit) map[string]any {
	result := map[string]any{"release": release, "replay_basis": "branch", "replay_runs_checked": 0, "replay_steps_checked": 0, "publication_started": false}
	if audit != nil {
		result["replay_basis"] = audit.Basis
		result["replay_runs_checked"] = audit.RunsChecked
		result["replay_steps_checked"] = audit.StepsChecked
		result["publication_started"] = audit.PublicationStarted
	}
	return result
}

func requireReplayAudit(release, force bool, audit *model.RunReplayAudit, evidence model.RunReplayEvidence) error {
	if !release {
		return nil
	}
	if audit == nil {
		return fmt.Errorf("%w: release replay basis is required", ErrInvalidState)
	}
	if force && audit.Basis == "forced" {
		return nil
	}
	if !force && evidence.ChainComplete && evidence.Newest && audit.RunsChecked == len(evidence.Chain) && (audit.Basis == "pre-intent" || audit.Basis == "observed-terminal-no-credential-start") {
		return nil
	}
	return fmt.Errorf("%w: release replay lacks complete safety evidence; use run_rerun with force", ErrInvalidState)
}
