package store

import (
	"context"
	"encoding/json"

	"github.com/oberthci/oberth/internal/stateport"
)

// RunState reads one summary/count/revision projection per waiter wake.
func (s *Store) RunState(ctx context.Context, id string) (stateport.RunState, error) {
	var result stateport.RunState
	var summary string
	err := s.readers.QueryRowContext(ctx, `SELECT summary,revision,step_count,steps_planned_exact,
steps_pending,steps_queued,steps_running,steps_passed,steps_failed,steps_skipped,steps_unknown
FROM runs WHERE id=?`, id).Scan(&summary, &result.Revision, &result.Total, &result.Exact, &result.Pending,
		&result.Queued, &result.Running, &result.Passed, &result.Failed, &result.Skipped, &result.Unknown)
	if err != nil {
		return result, databaseError(err)
	}
	counts := result.LogicalStepCounts
	if err := json.Unmarshal([]byte(summary), &result.RunSummary); err != nil {
		return result, refuse(corrupt, err)
	}
	result.LogicalStepCounts = counts
	return result, nil
}
