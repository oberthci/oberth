package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// SaveProgress persists the native projector's complete result without a control revision.
func (s *Store) SaveProgress(ctx context.Context, id string, p stateport.Progress) error {
	steps := p.Steps
	return s.observe(ctx, func(tx *sql.Tx) error {
		plan, err := readPlan(ctx, tx, id)
		if err != nil {
			return err
		}
		if !stateport.ValidNativeSteps(plan, steps) || publicerr.CheckMessage(p.Waiting, 40) != nil {
			return refuse(invalid, nil)
		}
		steps = stateport.PlanSteps(plan, steps)
		counts, err := stepmodel.CountSteps(steps, stateport.NativeStepsExact(plan, steps))
		if err != nil {
			return refuse(invalid, err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO native_steps VALUES(?,?) ON CONFLICT(run_id) DO UPDATE SET value=excluded.value`, id, encode(steps)); err != nil {
			return databaseError(err)
		}
		if err = recordProgress(ctx, tx, id, stateport.Progress{Steps: steps, Waiting: p.Waiting}); err != nil {
			return err
		}
		return saveCounts(ctx, tx, id, counts)
	})
}

// recordProgress sets the run's start from its earliest started step and, while
// the run is unfinished, its queued or running status and reason by the rule
// of stepmodel.Progress. A finished run keeps its status and reason.
func recordProgress(ctx context.Context, tx *sql.Tx, id string, p stateport.Progress) error {
	r, err := loadRun(ctx, tx, id)
	if err != nil {
		return err
	}
	next := r.RunSummary
	next.StartedAt = stepmodel.RunStart(r.StartedAt, p.Steps)
	if !terminal(r.Status) {
		next.Status, next.DisplayReason = stepmodel.Progress(r.Status, r.DisplayReason, p.Steps, p.Waiting)
	}
	if next.StartedAt == r.StartedAt && next.Status == r.Status && next.DisplayReason == r.DisplayReason {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE runs SET status=?,summary=? WHERE id=?`, next.Status, encode(next), id)
	return databaseError(err)
}

// Steps returns the saved projection verbatim, or the plan and evidence before observation.
func (s *Store) Steps(ctx context.Context, id string) ([]stepmodel.RunStep, error) {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	r, err := loadRun(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	var body string
	err = tx.QueryRowContext(ctx, `SELECT value FROM native_steps WHERE run_id=?`, id).Scan(&body)
	if err == nil {
		var steps []stepmodel.RunStep
		if err = json.Unmarshal([]byte(body), &steps); err != nil {
			return nil, refuse(corrupt, err)
		}
		return steps, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, databaseError(err)
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
		if _, err := readPlan(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE runs SET evidence_complete=0 WHERE id=?`, id)
		return databaseError(err)
	})
}
