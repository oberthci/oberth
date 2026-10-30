package store

import (
	"context"
	"fmt"
)

// QueuedRunPosition is admission order across all durable queued runs. It does
// not predict execution order, which can bypass publication-gated refs.
func (s *Store) QueuedRunPosition(ctx context.Context, id string) (int64, error) {
	var position int64
	err := s.db.QueryRowContext(ctx, `WITH queue AS (
 SELECT id, ROW_NUMBER() OVER (ORDER BY `+priorityOrderSQL(s.now())+`) AS position
 FROM runs WHERE status='queued' OR (status='running' AND started_at IS NULL)
) SELECT position FROM queue WHERE id=?`, id).Scan(&position)
	if err != nil {
		return 0, fmt.Errorf("read queued run position: %w", err)
	}
	return position, nil
}
