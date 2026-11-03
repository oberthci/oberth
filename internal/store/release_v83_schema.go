package store

import (
	"context"
	"fmt"
)

// The small v0.17.83 release owns v40. Keep this definition independent of the
// final candidate's later covering-index migration so published history is fixed.
const releaseV83MigrationVersion = 40
const releaseV83RunListSchema = `DROP INDEX IF EXISTS runs_latest_ref_idx;
CREATE INDEX IF NOT EXISTS runs_latest_ref_idx ON runs(repo_id, ref_kind, ref, queue_sequence DESC, status, started_at, finished_at, queued_at, id);`

func (s *Store) verifyReleaseV83Schema(ctx context.Context, querier schemaIdentityQuerier, expiryMigration, expiry int) error {
	var applied int
	if err := querier.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=?`, releaseV83MigrationVersion).Scan(&applied); err != nil {
		return fmt.Errorf("check v0.17.83 migration: %w", err)
	}
	if applied == 0 {
		return nil
	}
	if expiryMigration == 0 && expiry != 0 {
		return fmt.Errorf("%w: v40 has bearer expiry from the old candidate numbering; restore a compatible pre-candidate backup before starting Oberth", ErrSchemaIncompatible)
	}
	var columns string
	if err := querier.QueryRowContext(ctx, `SELECT COALESCE(group_concat(name || ':' || "desc", ','), '') FROM (SELECT name, "desc" FROM pragma_index_xinfo('runs_latest_ref_idx') WHERE key=1 ORDER BY seqno)`).Scan(&columns); err != nil {
		return fmt.Errorf("check v0.17.83 run-list index: %w", err)
	}
	if columns != "repo_id:0,ref_kind:0,ref:0,queue_sequence:1,status:0,started_at:0,finished_at:0,queued_at:0,id:0" {
		return fmt.Errorf("%w: migration v40 is recorded but the v0.17.83 run-list index is missing or incompatible; restore a compatible backup before starting Oberth", ErrSchemaIncompatible)
	}
	return nil
}
