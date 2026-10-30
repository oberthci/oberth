package store

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// OpenReadinessReaders is called once at server startup before sharing Store.
// Its single read-only WAL connection is independent of the writer, dashboard,
// audit, SSH authentication and idle sampler pools. Close owns this pool.
func (s *Store) OpenReadinessReaders(ctx context.Context) error {
	if s.readinessDB != nil {
		return nil
	}
	db, err := s.openLiveReaders(ctx, 1)
	if err != nil {
		return err
	}
	s.readinessDB = db
	return nil
}

func (s *Store) ListReadinessUpstreams(ctx context.Context) ([]model.Upstream, error) {
	if s.readinessDB == nil {
		return nil, fmt.Errorf("%w: readiness readers are not configured", ErrInvalidState)
	}
	return listUpstreams(ctx, s.readinessDB)
}
