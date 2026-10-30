package store

import (
	"context"
	"fmt"
)

// WriterHeartbeat exercises the production writer connection, SQLite write
// lock and commit path, including on an idle installation. The no-op UPDATE
// leaves migration metadata and the audit chain unchanged and needs no schema
// migration. It deliberately avoids the mutation gate, whose external witness
// services must not determine process liveness. TestWriterHeartbeat uses the
// real migrated schema: a future migration that forbids updates to this table
// must change the heartbeat target and its test together.
func (s *Store) WriterHeartbeat(ctx context.Context) error {
	if s.readOnly {
		return fmt.Errorf("%w: heartbeat requires a writer", ErrInvalidState)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE schema_migrations SET applied_at = applied_at WHERE version = 1`)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: heartbeat migration row is missing", ErrInvalidState)
	}
	return tx.Commit()
}
