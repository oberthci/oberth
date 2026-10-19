package store

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// MaxActiveRuns bounds ListActiveRuns. Active work is capped by the queue,
// not by retention, so one bounded page holds it in practice; a server with
// more than this many non-terminal runs shows the first MaxActiveRuns in the
// same order.
const MaxActiveRuns = 500

// ListActiveRuns returns every non-terminal run (queued, or claimed and
// running) matching the repository/ref filter and the uplink repository
// restriction, in the order the scheduler works through them:
//
//  1. runs whose Workflow has started, oldest start first;
//  2. claimed runs not yet started (preparing, waiting for admission, or
//     admitted and awaiting submission), in admission order: the weighted
//     admission gate reserves and grants in ascending queue_sequence, the
//     same order ClaimNextRun claims in;
//  3. unclaimed queued runs, in ascending queue_sequence (accepted order).
//
// Before, Limit and Terminal are ignored: the view is one bounded list.
func (s *Store) ListActiveRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	restriction, restrictionArgs := repoRestriction("repo_id", filter.RepoIDs)
	args := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref}, restrictionArgs...)
	rows, err := s.db.QueryContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE status IN ('queued', 'running')
  AND (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?)`+restriction+`
ORDER BY CASE
    WHEN status = 'running' AND started_at IS NOT NULL THEN 0
    WHEN status = 'running' THEN 1
    ELSE 2
  END, started_at, queue_sequence
LIMIT ?`, append(args, MaxActiveRuns)...) // #nosec G202 -- the restriction is a fixed column and ? placeholders from repoRestriction.
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	runs := make([]model.Run, 0)
	for rows.Next() {
		value, err := scanRun(rows)
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
