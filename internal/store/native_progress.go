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
	p.Steps = steps
	next := projectNativeProgress(before.run, p)
	next.LogicalStepCounts = counts
	sameSummary := encode(before.run.RunSummary) == encode(next.RunSummary)
	sameSteps, err := sameProgressSteps(before.steps, steps)
	if err != nil {
		return err
	}
	if sameSteps && sameSummary {
		return unchanged{}
	}
	if err := writeNativeProjection(ctx, tx, nativeProjectionWrite{id: id, steps: steps, summary: next.RunSummary, sameSummary: sameSummary}); err != nil {
		return err
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

func sameProgressSteps(previous string, steps []stepmodel.RunStep) (bool, error) {
	if previous == "" {
		return false, nil
	}
	if previous == encode(steps) {
		return true, nil
	}
	var before []stepmodel.RunStep
	if err := json.Unmarshal([]byte(previous), &before); err != nil {
		return false, refuse(corrupt, err)
	}
	return stateport.EqualStepFacts(before, steps), nil
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
		r.Diagnostic = p.DiagnosticFor(r.DisplayReason)
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

type nativeProjectionWrite struct {
	id          string
	steps       []stepmodel.RunStep
	summary     stepmodel.RunSummary
	sameSummary bool
}

func writeNativeProjection(ctx context.Context, tx *sql.Tx, row nativeProjectionWrite) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO native_steps VALUES(?,?) ON CONFLICT(run_id) DO UPDATE SET value=excluded.value`, row.id, encode(row.steps)); err != nil {
		return databaseError(err)
	}
	if !row.sameSummary {
		return writeProgressSummary(ctx, tx, row.id, row.summary)
	}
	return nil
}
