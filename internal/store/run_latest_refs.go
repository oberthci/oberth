package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// listLatestRefRuns lists each ref's newest run in the same three groups as
// the flat list: queued heads with the run admitted next at the bottom,
// running heads most recently started first, then terminal heads most recently
// finished first. The first page carries every queued and running head; only
// terminal heads count against the page limit and page by a fixed (end time,
// ID) cursor, so no limit or page ever drops a queued or running head below a
// finished one. Repo scope filters both counts and heads, so the aggregate
// cannot reveal hidden work.
func (s *Store) listLatestRefRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	tx, err := s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin latest ref snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var beforeTime int64
	var beforeID string
	if filter.BeforeRun != "" || filter.Before > 0 {
		cursor, err := s.runListCursor(ctx, tx, filter)
		if err != nil {
			return nil, err
		}
		beforeID = cursor.ID
		beforeTime = unixNano(runEndTime(cursor))
	}
	// A latest-per-ref head cannot have a newer run of that same ref.
	if filter.Superseded {
		return []model.Run{}, nil
	}
	restriction, args := repoRestriction("repo_id", filter.RepoIDs)
	params := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref}, args...)
	params = append(params, beforeID, MaxActiveRuns, beforeID, beforeTime, beforeTime, beforeID, pageLimit(filter.Limit))
	// Group using a narrow covering index: only sequences/counts are scanned,
	// never historical errors/logs. Fetch the wide rows after selecting heads.
	// Queue positions use the same global priority order as the flat list.
	rows, err := tx.QueryContext(ctx, latestRefQuery(restriction, priorityOrderSQL(s.now())), params...) //nolint:gosec // Fixed query with placeholder-only scope restrictions.
	if err != nil {
		return nil, fmt.Errorf("list latest refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []model.Run{}
	for rows.Next() {
		var earlier int
		var position int64
		run, err := scanReadRun(rows, &earlier, &position)
		if err != nil {
			return nil, err
		}
		run.EarlierAttempts, run.QueuePosition = earlier, position
		result = append(result, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list latest refs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close latest refs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit latest ref snapshot: %w", err)
	}
	return result, nil
}

// latestRefQuery ranks every head into a list group (0 queued, 1 running,
// 2 terminal) before paging, so an older page can never resurrect an older
// attempt of a ref whose head appeared earlier, and never carries active work.
func latestRefQuery(restriction, priorityOrder string) string {
	return `WITH heads AS (
 SELECT MAX(queue_sequence) AS head_sequence, COUNT(*)-1 AS earlier_attempts
 FROM runs INDEXED BY runs_latest_ref_idx
 WHERE (?=0 OR repo_id=?) AND (?='' OR ref=?)` + restriction + `
 GROUP BY repo_id,ref_kind,ref
), queue AS (
 SELECT id AS queued_run_id, ROW_NUMBER() OVER(ORDER BY ` + priorityOrder + `) AS queue_position
 FROM runs WHERE ` + queuedRunSQL + `
), ranked AS (
 SELECT runs.*, heads.earlier_attempts AS earlier_attempts,
  COALESCE(queue.queue_position,0) AS queue_position,
  CASE WHEN queue.queue_position IS NOT NULL THEN 0 WHEN runs.status='running' THEN 1 ELSE 2 END AS list_group,
  ` + runEndTimeSQL + ` AS end_time
 FROM heads JOIN runs ON runs.queue_sequence=heads.head_sequence
 LEFT JOIN queue ON queue.queued_run_id=runs.id
), page AS (
 SELECT * FROM (SELECT * FROM ranked WHERE list_group<2 AND ?='' ORDER BY list_group, queue_position DESC, started_at DESC, id DESC LIMIT ?)
 UNION ALL
 SELECT * FROM (SELECT * FROM ranked WHERE list_group=2 AND (?='' OR end_time<? OR (end_time=? AND id<?)) ORDER BY end_time DESC, id DESC LIMIT ?)
)
 SELECT ` + runReadColumns + `,earlier_attempts,queue_position FROM page AS runs
 ORDER BY list_group, queue_position DESC, CASE list_group WHEN 1 THEN started_at ELSE end_time END DESC, id DESC`
}
