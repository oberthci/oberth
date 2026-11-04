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
//  2. every run that has not started, claimed or not, as one queue in the
//     scheduler's own order (priorityOrderSQL: effective class, then
//     admission_sequence, then queue_sequence), the same order ClaimNextRun
//     claims in and the weighted gate grants in. A claimed normal run never
//     lists ahead of an unclaimed high run the gate will admit first; the
//     flat and latest-per-ref lists number this same queue.
//
// Before, Limit and Terminal are ignored: the view is one bounded list.
func (s *Store) ListActiveRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	statusSQL, statusArgs, err := runStatusRestriction(filter.Status)
	if err != nil {
		return nil, err
	}
	restriction, restrictionArgs := repoRestriction("repo_id", filter.RepoIDs)
	restriction += statusSQL
	restrictionArgs = append(restrictionArgs, statusArgs...)
	args := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref}, restrictionArgs...)
	priorityOrder := priorityOrderSQL(s.now())
	rows, err := s.reader().QueryContext(ctx, `
WITH queue AS (
 SELECT id AS queued_id, ROW_NUMBER() OVER (ORDER BY `+priorityOrder+`) AS queue_position FROM runs
 WHERE status = 'queued' OR (status = 'running' AND started_at IS NULL)
)
SELECT `+runReadColumns+`, COALESCE(queue_position, 0) FROM runs LEFT JOIN queue ON runs.id = queue.queued_id
WHERE status IN ('queued', 'running')
  AND (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?)`+restriction+supersededRunRestriction(filter)+`
ORDER BY CASE WHEN status = 'running' AND started_at IS NOT NULL THEN 0 ELSE 1 END, started_at, `+priorityOrder+`
LIMIT ?`, append(args, MaxActiveRuns)...) // #nosec G202 -- the restriction is a fixed column and ? placeholders from repoRestriction.
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	runs := make([]model.Run, 0)
	for rows.Next() {
		var position int64
		value, err := scanReadRun(rows, &position)
		value.QueuePosition = position
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
