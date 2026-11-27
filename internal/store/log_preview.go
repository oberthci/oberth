package store

import (
	"context"
)

// LogProtection reports current durable blockers through the existing retention
// owner. It grants no authority to delete, even when no current blocker exists.
func (s *Store) LogProtection(ctx context.Context, id string) (string, error) {
	var reason string
	err := s.readers.QueryRowContext(ctx, `SELECT CASE
WHEN status IN ('queued','running') THEN 'run is active'
WHEN EXISTS(SELECT 1 FROM obligations WHERE run_id=r.id AND value<>'') THEN 'durable work protects logs'
WHEN EXISTS(SELECT 1 FROM publications WHERE run_id=r.id AND status='pending') THEN 'publication protects logs'
WHEN EXISTS(SELECT 1 FROM runs child INDEXED BY run_ref WHERE child.repo_id=r.repo_id AND child.ref=r.ref AND json_extract(child.origin,'$.resumed_from')=r.id) THEN 'resume lineage protects logs'
ELSE 'archive eligibility unknown' END FROM runs r WHERE id=?`, id).Scan(&reason)
	return reason, databaseError(err)
}
