package store

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// Rank before applying the cursor: older pages must never resurrect an older
// attempt of a ref whose current head appeared on an earlier page. Repo scope
// filters both counts and heads, so the aggregate cannot reveal hidden work.
func (s *Store) listLatestRefRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	if filter.BeforeRun != "" {
		cursor, err := s.Run(ctx, filter.BeforeRun)
		if err != nil {
			return nil, err
		}
		filter.Before = cursor.QueueSequence
	}
	restriction, args := repoRestriction("repo_id", filter.RepoIDs)
	params := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref}, args...)
	params = append(params, filter.Before, filter.Before, pageLimit(filter.Limit))
	// Group using a narrow covering index: only sequences/counts are scanned,
	// never historical errors/logs. Fetch the wide rows after selecting heads.
	// Queue positions use the same global priority order as the flat list.
	rows, err := s.reader().QueryContext(ctx, latestRefQuery(restriction, priorityOrderSQL(s.now())), params...) //nolint:gosec // Fixed query with placeholder-only scope restrictions.

	if err != nil {
		return nil, fmt.Errorf("list latest refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []model.Run{}
	for rows.Next() {
		var earlier int
		var position int64
		run, err := scanRun(rows, &earlier, &position)
		if err != nil {
			return nil, err
		}
		run.EarlierAttempts, run.QueuePosition = earlier, position
		result = append(result, run)
	}
	return result, rows.Err()
}

func latestRefQuery(restriction, priorityOrder string) string {
	return `WITH heads AS (
 SELECT MAX(queue_sequence) AS head_sequence, COUNT(*)-1 AS earlier_attempts
 FROM runs INDEXED BY runs_latest_ref_idx
 WHERE (?=0 OR repo_id=?) AND (?='' OR ref=?)` + restriction + `
 GROUP BY repo_id,ref_kind,ref
), selected AS MATERIALIZED (
 SELECT head_sequence,earlier_attempts FROM heads
 WHERE (?<=0 OR head_sequence<?) ORDER BY head_sequence DESC LIMIT ?
), queue AS (
 SELECT id AS queued_run_id, ROW_NUMBER() OVER(ORDER BY ` + priorityOrder + `) AS queue_position
 FROM runs WHERE status='queued' OR (status='running' AND started_at IS NULL)
)
 SELECT ` + runColumns + `,heads.earlier_attempts,COALESCE(queue.queue_position,0)
 FROM selected AS heads JOIN runs ON runs.queue_sequence=heads.head_sequence
 LEFT JOIN queue ON queue.queued_run_id=runs.id
 ORDER BY runs.queue_sequence DESC`
}
