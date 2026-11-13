package storetest

import (
	"context"
	"encoding/json"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// PutHistoricalStepResult reconstructs retained evidence for read/legacy fixtures.
// It deliberately bypasses the live writer, which refuses terminal rewrites.
func PutHistoricalStepResult(database *store.Store, ctx context.Context, result model.StepResult) (model.StepResult, error) {
	db, fixture, err := fixtureDB(database)
	if err != nil {
		return model.StepResult{}, err
	}
	defer func() { _ = db.Close() }()
	var diagnostic any
	if result.Termination != nil {
		body, err := json.Marshal(result.Termination)
		if err != nil {
			return model.StepResult{}, err
		}
		diagnostic = string(body)
	}
	stamp := func(value *time.Time) any {
		if value == nil {
			return nil
		}
		return value.UnixNano()
	}
	_, err = db.ExecContext(ctx, `INSERT INTO step_results(
 run_id,burn,step,ordinal,status,exit_code,log_start,log_end,
 started_at,finished_at,recorded_at,termination_json)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(run_id,burn,step) DO UPDATE SET ordinal=excluded.ordinal,status=excluded.status,
 exit_code=excluded.exit_code,log_start=excluded.log_start,log_end=excluded.log_end,
 started_at=excluded.started_at,finished_at=excluded.finished_at,
 recorded_at=excluded.recorded_at,termination_json=excluded.termination_json`,
		result.RunID, result.Stage, result.Step, result.Ordinal, result.Status, result.ExitCode, result.LogStart, result.LogEnd,
		stamp(result.StartedAt), stamp(result.FinishedAt), fixtureNow(fixture).UnixNano(), diagnostic)
	return result, err
}
