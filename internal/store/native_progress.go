package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// SaveProgress persists the native projector's complete result without a control revision.
func (s *Store) SaveProgress(ctx context.Context, id string, p stateport.Progress) error {
	return s.observe(ctx, func(tx *sql.Tx) error {
		return s.saveNativeProgress(ctx, tx, id, p)
	})
}

func (s *Store) saveNativeProgress(ctx context.Context, tx *sql.Tx, id string, p stateport.Progress) error {
	steps := p.Steps
	before, err := mutableProgress(ctx, tx, id)
	if err != nil {
		return err
	}
	if !before.run.HasAdmission {
		return refuse(noAdmission, nil)
	}
	plan := before.run.Admission.Plan
	if !stateport.ValidNativeSteps(plan, steps) || publicerr.CheckMessage(p.Waiting, 40) != nil {
		return refuse(invalid, nil)
	}
	steps = stateport.PlanSteps(plan, steps)
	counts, err := stepmodel.CountSteps(steps, stateport.NativeStepsExact(plan, steps))
	if err != nil {
		return refuse(invalid, err)
	}
	next := projectNativeProgress(before.run, stateport.Progress{Steps: steps, Waiting: p.Waiting})
	next.LogicalStepCounts = counts
	sameSummary := encode(before.run.RunSummary) == encode(next.RunSummary)
	if before.steps == encode(steps) && sameSummary {
		return unchanged{}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO native_steps VALUES(?,?) ON CONFLICT(run_id) DO UPDATE SET value=excluded.value`, id, encode(steps)); err != nil {
		return databaseError(err)
	}
	if !sameSummary {
		if err = writeProgressSummary(ctx, tx, id, next.RunSummary); err != nil {
			return err
		}
	}
	return s.progressEvent(ctx, tx, next)
}

func mutableProgress(ctx context.Context, tx *sql.Tx, id string) (progressSnapshot, error) {
	before, err := readProgress(ctx, tx, id)
	if err != nil {
		return before, err
	}
	if terminal(before.run.Status) {
		err = requireUnhandedRun(ctx, tx, id)
	}
	return before, err
}

func writeProgressSummary(ctx context.Context, tx *sql.Tx, id string, summary stepmodel.RunSummary) error {
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,summary=? WHERE id=?`, summary.Status, encode(summary), id); err != nil {
		return databaseError(err)
	}
	return saveCounts(ctx, tx, id, summary.LogicalStepCounts)
}

// projectNativeProgress sets the run's start from its earliest started step and, while
// the run is unfinished, its queued or running status and reason by the rule
// of stepmodel.Progress. A finished run keeps its status and reason.
func projectNativeProgress(r stateport.Run, p stateport.Progress) stateport.Run {
	r.StartedAt = stepmodel.RunStart(r.StartedAt, p.Steps)
	if !terminal(r.Status) {
		r.Status, r.DisplayReason = stepmodel.Progress(r.Status, r.DisplayReason, p.Steps, p.Waiting)
	}
	return r
}

// Steps returns the saved projection verbatim, or the plan and evidence before observation.
func (s *Store) Steps(ctx context.Context, id string) ([]stepmodel.RunStep, error) {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	var body sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT n.value FROM runs r LEFT JOIN native_steps n ON n.run_id=r.id WHERE r.id=?`, id).Scan(&body)
	if err != nil {
		return nil, databaseError(err)
	}
	if body.Valid {
		var steps []stepmodel.RunStep
		if err = json.Unmarshal([]byte(body.String), &steps); err != nil {
			return nil, refuse(corrupt, err)
		}
		return steps, nil
	}
	r, err := readRun(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	if r.HasAdmission {
		r.Admission.Plan, err = readPlan(ctx, tx, id)
		if err != nil {
			return nil, err
		}
	}
	nodes, err := readNodes(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return stateport.ProjectSteps(r, nodes), nil
}

// Nodes returns independently retained native evidence from one snapshot.
func (s *Store) Nodes(ctx context.Context, id string) ([]stateport.Node, error) {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = loadRun(ctx, tx, id); err != nil {
		return nil, err
	}
	return readNodes(ctx, tx, id)
}

// AdvanceNative invalidates evidence completion without changing the terminal result.
func (s *Store) AdvanceNative(ctx context.Context, id string) error {
	return s.observe(ctx, func(tx *sql.Tx) error {
		if err := requireUnhandedRun(ctx, tx, id); err != nil {
			return err
		}
		if err := s.runChange(ctx, tx, id); err != nil {
			return err
		}
		if _, err := readPlan(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE runs SET evidence_complete=0 WHERE id=?`, id)
		return databaseError(err)
	})
}
