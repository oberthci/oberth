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
 run_id,node_id,display_name,template,node_type,boundary_id,phase,message,pod_name,burn,step,ordinal,status,exit_code,
 started_at,finished_at,recorded_at,termination_json)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(run_id,node_id) DO UPDATE SET ordinal=excluded.ordinal,status=excluded.status,
 exit_code=excluded.exit_code,started_at=excluded.started_at,finished_at=excluded.finished_at,
 recorded_at=excluded.recorded_at,termination_json=excluded.termination_json`,
		result.RunID, result.NodeID, result.DisplayName, result.Template, result.NodeType, result.BoundaryID, result.Phase, result.Message, result.PodName,
		result.Stage, result.Step, result.Ordinal, result.Status, result.ExitCode,
		stamp(result.StartedAt), stamp(result.FinishedAt), fixtureNow(fixture).UnixNano(), diagnostic)
	return result, err
}
