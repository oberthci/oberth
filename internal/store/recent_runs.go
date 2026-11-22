package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

// recentRunsQuery walks the run_history index once per named repository and
// stops after its newest runs, so the read costs the same however long a
// history is. Rows come oldest first within each repository.
const recentRunsQuery = `SELECT repositories.canonical,runs.id,runs.status,runs.ref,json_extract(runs.summary,'$.created_at')
FROM repositories JOIN runs ON runs.sequence IN(SELECT recent.sequence FROM runs AS recent
WHERE recent.repo_id=repositories.id ORDER BY recent.sequence DESC LIMIT ?)
WHERE `

// RecentRuns returns each named repository's newest RecentRunLimit runs, oldest
// first, in one read. The names are the caller's scope: a repository outside
// them is never read, and a named repository without runs has no entry.
func (s *Store) RecentRuns(ctx context.Context, repositories []string) (map[string][]stateport.RunMark, error) {
	if repositories == nil || len(repositories) > stateport.RunPageLimit {
		return nil, refuse(invalid, nil)
	}
	predicate, args := scopePredicate("repositories.canonical", repositories)
	rows, err := s.readers.QueryContext(ctx, recentRunsQuery+predicate+` ORDER BY repositories.id,runs.sequence`,
		append([]any{stateport.RecentRunLimit}, args...)...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]stateport.RunMark{}
	for rows.Next() {
		var repo string
		var created sql.NullString
		var mark stateport.RunMark
		if err := rows.Scan(&repo, &mark.ID, &mark.Status, &mark.Ref, &created); err != nil {
			return nil, databaseError(err)
		}
		if created.Valid {
			if mark.CreatedAt, err = time.Parse(time.RFC3339Nano, created.String); err != nil {
				return nil, refuse(corrupt, err)
			}
		}
		out[repo] = append(out[repo], mark)
	}
	return out, databaseError(rows.Err())
}
