package store

import (
	"context"
	"time"
)

// openAuthReaders uses the same startup-cached read-only WAL policy as audit
// verification. The Store options already supplied the shared busy timeout.
func (s *Store) openAuthReaders(ctx context.Context, _ time.Duration) error {
	db, err := s.openLiveReaders(ctx, 4)
	if err != nil {
		return err
	}
	s.authDB = db
	return nil
}
