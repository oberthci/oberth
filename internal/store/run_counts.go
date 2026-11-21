package store

import (
	"context"

	"github.com/oberthci/oberth/internal/stepmodel"
)

func (s *Store) RunCounts(ctx context.Context, scope []string) (map[stepmodel.RunStatus]int, error) {
	predicate, args := scopePredicate("repositories.canonical", scope)
	rows, err := s.readers.QueryContext(ctx, `SELECT runs.status,count(*) FROM runs JOIN repositories ON repositories.id=runs.repo_id WHERE `+predicate+` GROUP BY runs.status`, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	counts := map[stepmodel.RunStatus]int{}
	for rows.Next() {
		var status stepmodel.RunStatus
		var n int
		if err = rows.Scan(&status, &n); err != nil {
			return nil, databaseError(err)
		}
		counts[status] = n
	}
	return counts, databaseError(rows.Err())
}
