package store

import (
	"context"
	"fmt"
)

// LegacyRetirementBlockers leaves every journal intact. An idle queue alone
// does not establish that an externally owned resource has been cleaned up.
func (s *Store) LegacyRetirementBlockers(ctx context.Context) ([]string, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT label FROM (
SELECT 'execution ' || run_id AS label FROM vm_executions WHERE cleaned=0
UNION SELECT 'capacity ' || run_id FROM vm_capacity_slots
UNION SELECT 'suite ' || run_id FROM vm_suite_executions WHERE cleaned=0
UNION SELECT 'suite resource ' || run_id FROM vm_suite_resources WHERE cleaned=0
UNION SELECT 'run ' || id FROM runs WHERE status IN ('queued','running')
UNION SELECT 'cancellation ' || run_id FROM run_cancellations WHERE completed_at IS NULL
) ORDER BY label LIMIT 21`)
	if err != nil {
		return nil, fmt.Errorf("inspect retirement journals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 20 {
		out[20] = "additional obligations remain"
	}
	return out, nil
}
