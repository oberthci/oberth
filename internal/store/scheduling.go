package store

import (
	"context"
	"fmt"
)

// QueuedRunPosition is accepted order across all durable queued runs. It does
// not predict claim order, which can bypass publication-gated refs.
func (s *Store) QueuedRunPosition(ctx context.Context, id string) (int64, error) {
	var position int64
	err := s.db.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM runs AS earlier WHERE earlier.status = 'queued'
  AND earlier.queue_sequence <= candidate.queue_sequence)
 FROM runs AS candidate WHERE candidate.id = ? AND candidate.status = 'queued'`, id).Scan(&position)
	if err != nil {
		return 0, fmt.Errorf("read queued run position: %w", err)
	}
	return position, nil
}
