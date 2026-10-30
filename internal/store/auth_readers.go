package store

import (
	"context"
	"database/sql"
)

// OpenSSHAuthReaders opens reserved readers for SSH and bearer authentication,
// isolated from both the serialized writer and ordinary dashboard/list reads.
// The long-running server calls this once before sharing the Store with any
// goroutines. CLI handles use the ordinary reader pool; inspection handles
// retain their private primary connection.
// The shared live-reader opener uses the Store's startup-cached read-only WAL
// policy and busy timeout. Close owns the reader pool.
func (s *Store) OpenSSHAuthReaders(ctx context.Context) error {
	if s.authDB != nil {
		return nil
	}
	db, err := s.openLiveReaders(ctx, 4)
	if err != nil {
		return err
	}
	s.authDB = db
	return nil
}

func (s *Store) authReader() *sql.DB {
	if s.authDB != nil {
		return s.authDB
	}
	// CLI, inspection handles and fixtures retain their existing reader policy.
	return s.reader()
}
