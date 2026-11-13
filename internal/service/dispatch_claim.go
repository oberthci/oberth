package service

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
)

func (s *Scheduler) claimDispatchRun(ctx context.Context, _ int) (model.Run, error) {
	if err := s.awaitDispatchMutation(ctx); err != nil {
		return model.Run{}, err
	}
	return s.database().ClaimNextRun(ctx)
}
