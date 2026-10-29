package store

import "context"

// OpenSSHAuthReaders isolates SSH identity reads from the serialized writer.
// The long-running server calls this once before sharing the Store with any
// goroutines. CLI and inspection handles use only the primary connection.
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
