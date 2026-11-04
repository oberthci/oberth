package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// runListOrderMigrationVersion widens the latest-ref index so one covering
// scan yields each head's status and end time: SQLite takes bare columns
// from the row holding MAX(queue_sequence). Ranking every ref's head then
// needs no per-head row read (the wide error text sits before those columns),
// which keeps the grouped latest-per-ref page as cheap as the
// sequence-ordered one it replaces.
const runListOrderMigrationVersion = 52
const runListOrderSchema = `DROP INDEX IF EXISTS runs_latest_ref_idx;
CREATE INDEX IF NOT EXISTS runs_latest_ref_idx ON runs(repo_id, ref_kind, ref, queue_sequence DESC, status, started_at, finished_at, queued_at, id);`

// listLatestRefRuns lists each ref's newest run in the same three groups as
// the flat list: queued heads in scheduler claim order,
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
	statusSQL, statusArgs, err := runStatusRestriction(filter.Status)
	if err != nil {
		return nil, err
	}
	restriction, args := repoRestriction("repo_id", filter.RepoIDs)
	params := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref}, args...)
	params = append(params, statusArgs...)
	params = append(params, beforeID, MaxActiveRuns, beforeID, beforeTime, beforeTime, beforeID, pageLimit(filter.Limit))
	// Group using a narrow covering index: only sequences/counts are scanned,
	// never historical errors/logs. Fetch the wide rows after selecting heads.
	// Queue positions use the same global priority order as the flat list.
	rows, err := tx.QueryContext(ctx, latestRefQuery(restriction, priorityOrderSQL(s.now()), statusSQL), params...) //nolint:gosec // Fixed query with placeholder-only scope restrictions.
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
// The ranking reads the covering index only; the wide rows, with their
// priority subqueries and queue positions, are fetched for the selected page.
func latestRefQuery(restriction, priorityOrder string, statusSQL ...string) string {
	heads := "heads"
	filtered := ""
	if len(statusSQL) > 0 && statusSQL[0] != "" {
		filtered = `, filtered_heads AS (SELECT heads.* FROM heads JOIN runs ON runs.queue_sequence=heads.head_sequence WHERE 1=1` + statusSQL[0] + `)`
		heads = "filtered_heads"
	}
	return `WITH heads AS (
 SELECT MAX(queue_sequence) AS head_sequence, id AS head_id, COUNT(*)-1 AS earlier_attempts, started_at AS head_started_at,
  ` + runEndTimeSQL + ` AS end_time,
  CASE WHEN ` + queuedRunSQL + ` THEN 0 WHEN status='running' THEN 1 ELSE 2 END AS list_group
 FROM runs INDEXED BY runs_latest_ref_idx
 WHERE (?=0 OR repo_id=?) AND (?='' OR ref=?)` + restriction + `
 GROUP BY repo_id,ref_kind,ref
)` + filtered + `, page AS (
 SELECT * FROM (SELECT * FROM ` + heads + ` WHERE list_group<2 AND ?='' ORDER BY head_sequence DESC LIMIT ?)
 UNION ALL
 SELECT * FROM (SELECT * FROM ` + heads + ` WHERE list_group=2 AND (?='' OR end_time<? OR (end_time=? AND head_id<?)) ORDER BY end_time DESC, head_id DESC LIMIT ?)
), queue AS MATERIALIZED (
 SELECT id AS queued_run_id, ROW_NUMBER() OVER(ORDER BY ` + priorityOrder + `) AS queue_position
 FROM runs WHERE ` + queuedRunSQL + `
)
 SELECT ` + runReadColumns + `,page.earlier_attempts,COALESCE(queue.queue_position,0)
 FROM page JOIN runs ON runs.queue_sequence=page.head_sequence
 LEFT JOIN queue ON queue.queued_run_id=runs.id
 ORDER BY page.list_group, COALESCE(queue.queue_position,0) ASC, CASE page.list_group WHEN 1 THEN page.head_started_at ELSE page.end_time END DESC, page.head_id DESC`
}
