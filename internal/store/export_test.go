package store

import (
	"context"
	"database/sql"
)

// HoldWriterForTest reserves the real serialized writer for external HTTP
// integration tests. It is compiled only into the Store test binary.
func HoldWriterForTest(ctx context.Context, s *Store) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}
