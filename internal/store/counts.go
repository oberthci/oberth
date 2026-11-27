package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func readPlan(ctx context.Context, tx *sql.Tx, runID string) (admissioncontract.Plan, error) {
	var body sql.NullString
	var plan admissioncontract.Plan
	err := tx.QueryRowContext(ctx, `SELECT json_extract(admission,'$.plan') FROM runs WHERE id=?`, runID).Scan(&body)
	if err != nil {
		return plan, databaseError(err)
	}
	if !body.Valid {
		return plan, refuse(noAdmission, nil)
	}
	if err := json.Unmarshal([]byte(body.String), &plan); err != nil {
		return plan, refuse(corrupt, err)
	}
	return plan, nil
}

func readCounts(ctx context.Context, tx *sql.Tx, id string) (stepmodel.LogicalStepCounts, error) {
	var c stepmodel.LogicalStepCounts
	err := tx.QueryRowContext(ctx, `SELECT step_count,steps_planned_exact,steps_pending,steps_queued,
  steps_running,steps_passed,steps_failed,steps_skipped,steps_unknown FROM runs WHERE id=?`, id).
		Scan(&c.Total, &c.Exact, &c.Pending, &c.Queued, &c.Running, &c.Passed, &c.Failed, &c.Skipped, &c.Unknown)
	return c, databaseError(err)
}

const progressCountsSQL = `UPDATE runs SET step_count=?,steps_planned_exact=?,steps_pending=?,
  steps_queued=?,steps_running=?,steps_passed=?,steps_failed=?,steps_skipped=?,steps_unknown=? WHERE id=?`

func progressCountsArgs(id string, c stepmodel.LogicalStepCounts) []any {
	return []any{c.Total, c.Exact, c.Pending, c.Queued, c.Running, c.Passed, c.Failed, c.Skipped, c.Unknown, id}
}

func saveCounts(ctx context.Context, tx *sql.Tx, id string, c stepmodel.LogicalStepCounts) error {
	_, err := tx.ExecContext(ctx, progressCountsSQL, progressCountsArgs(id, c)...)
	return databaseError(err)
}

func resolveRemainders(ctx context.Context, tx *sql.Tx, runID string) error {
	var saved bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_steps WHERE run_id=?)`, runID).Scan(&saved); err != nil {
		return databaseError(err)
	}
	if saved {
		return nil
	}
	plan, err := readPlan(ctx, tx, runID)
	if err != nil {
		return err
	}
	counts, err := readCounts(ctx, tx, runID)
	if err != nil {
		return err
	}
	for _, p := range plan.Entries {
		if p.Dynamic {
			counts.Pending--
			counts.Unknown++
		}
	}
	return saveCounts(ctx, tx, runID, counts)
}
