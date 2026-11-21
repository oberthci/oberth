package store

import (
	"context"
)

// RepositoryScope resolves authenticated upstream scope once, before service reads.
func (s *Store) RepositoryScope(ctx context.Context, upstreams []string) ([]string, error) {
	if upstreams == nil {
		return nil, ctx.Err()
	}
	result := make([]string, 0)
	if len(upstreams) == 0 {
		return result, ctx.Err()
	}
	predicate, args := scopePredicate("upstream", upstreams)
	rows, err := s.readers.QueryContext(ctx, `SELECT canonical FROM repositories WHERE `+predicate+` ORDER BY canonical`, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, databaseError(err)
		}
		result = append(result, name)
	}
	return result, databaseError(rows.Err())
}
