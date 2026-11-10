package store

import (
	"context"
	"fmt"
)

// ActiveRunIDs lists the runs that are queued, waiting or running. A
// rollback restore hands them to the server as restored so none is
// dispatched again (internal/service/restored_runs.go).
func (s *Store) ActiveRunIDs(ctx context.Context) ([]string, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT id FROM runs WHERE status IN ('queued','waiting','running') ORDER BY queue_sequence`)
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan active run: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
