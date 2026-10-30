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
	rows, err := s.reader().QueryContext(ctx, `WITH heads AS (
 SELECT *,ROW_NUMBER() OVER(PARTITION BY repo_id,ref_kind,ref ORDER BY queue_sequence DESC) AS ref_rank,
 COUNT(*) OVER(PARTITION BY repo_id,ref_kind,ref)-1 AS earlier_attempts
 FROM runs WHERE (?=0 OR repo_id=?) AND (?='' OR ref=?)`+restriction+`)
 SELECT `+runColumns+`,earlier_attempts FROM heads AS runs
 WHERE ref_rank=1 AND (?<=0 OR queue_sequence<?) ORDER BY queue_sequence DESC LIMIT ?`, params...) //nolint:gosec // Fixed query with placeholder-only scope restrictions.
	if err != nil {
		return nil, fmt.Errorf("list latest refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []model.Run{}
	for rows.Next() {
		var earlier int
		run, err := scanRun(rows, &earlier)
		if err != nil {
			return nil, err
		}
		run.EarlierAttempts = earlier
		result = append(result, run)
	}
	return result, rows.Err()
}
