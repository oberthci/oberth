package store

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// MaxActiveRuns bounds this read-only view to 500 rows. Active execution is
// unbounded by Oberth; larger installations receive the first page in the
// documented started/submission order.
const MaxActiveRuns = 500

// ListActiveRuns returns non-terminal history matching the repository/ref
// filter and the uplink repository restriction:
//
//  1. runs whose Workflow has started, oldest start first;
//  2. runs without an execution start, in acceptance order.
//
// Argo controls execution parallelism after submission.
// Before, Limit and Terminal are ignored: the view is one bounded list.
func (s *Store) ListActiveRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	statusSQL, statusArgs, err := runStatusRestriction(filter.Status)
	if err != nil {
		return nil, err
	}
	restriction, restrictionArgs := runRestriction(filter)
	restriction += statusSQL
	restrictionArgs = append(restrictionArgs, statusArgs...)
	args := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref}, restrictionArgs...)
	orderSQL := "queue_sequence"
	rows, err := s.reader().QueryContext(ctx, `
SELECT `+runReadColumns+` FROM runs
WHERE status IN ('queued', 'running')
  AND (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?)`+restriction+`
ORDER BY CASE WHEN status = 'running' AND started_at IS NOT NULL THEN 0 ELSE 1 END, started_at, `+orderSQL+`
LIMIT ?`, append(args, MaxActiveRuns)...) // #nosec G202 -- the restriction is a fixed column and ? placeholders from repoRestriction.
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	runs := make([]model.Run, 0)
	for rows.Next() {
		value, err := scanReadRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	return runs, nil
}
