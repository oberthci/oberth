package store

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// EnqueueRun retains the cross-package historical fixture entry point. Live
// receives use EnqueueReceiveEvent and its actor-bound object validation.
func (s *Store) EnqueueRun(ctx context.Context, spec model.RunSpec) (model.EnqueueRunResult, error) {
	spec, err := validateRunSpec(spec)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("generate run id: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("begin enqueue run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := s.enqueueRunTx(ctx, tx, spec, id, now, 0)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("commit enqueue run: %w", err)
	}
	return result, nil
}
