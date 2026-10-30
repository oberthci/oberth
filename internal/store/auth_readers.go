package store

import (
	"context"
	"database/sql"
)

const authReadConnections = 4

// Neither protocol may occupy the entire shared reserve. Slots include both
// active reads and database/sql waiters, leaving the other protocol capacity.
const authProtocolSlots = authReadConnections - 1

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
	db, err := s.openLiveReaders(ctx, authReadConnections)
	if err != nil {
		return err
	}
	s.sshAuthSlots = make(chan struct{}, authProtocolSlots)
	s.bearerAuthSlots = make(chan struct{}, authProtocolSlots)
	s.authDB = db
	return nil
}

func acquireAuthSlot(ctx context.Context, slots chan struct{}) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if slots == nil {
		// CLI and inspection handles do not open the server auth reserve.
		return func() {}, nil
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Store) authReader() *sql.DB {
	if s.authDB != nil {
		return s.authDB
	}
	// CLI, inspection handles and fixtures retain their existing reader policy.
	return s.reader()
}
