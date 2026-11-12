package service

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func (s *Scheduler) claimDispatchRun(ctx context.Context, active int) (model.Run, error) {
	matching, supportsMatching := s.store.(interface {
		ClaimNextRunMatching(context.Context, func(model.Run) bool) (model.Run, error)
	})
	_, routesPools := s.jobs.(interface {
		AdoptedAdmissionCPU(context.Context, model.Run) (argoworkflow.CPUAdmission, error)
	})
	if !supportsMatching || !routesPools {
		if !s.dispatchSlotAvailable(active) {
			available, err := s.priorityDispatchAvailable(ctx, active)
			if err != nil {
				return model.Run{}, err
			}
			if !available {
				return model.Run{}, store.ErrNotFound
			}
		}
	}
	if err := s.awaitDispatchMutation(ctx); err != nil {
		return model.Run{}, err
	}
	if err := s.awaitDispatchAdmission(ctx); err != nil {
		return model.Run{}, err
	}
	if supportsMatching && routesPools {
		return s.database().claimRun(ctx, func(ctx context.Context) (model.Run, error) {
			return matching.ClaimNextRunMatching(ctx, func(run model.Run) bool {
				pool, known := s.reservationCPU(ctx, run)
				if !known {
					return false
				}
				s.admission.mu.Lock()
				defer s.admission.mu.Unlock()
				return s.admission.claimSlotLocked(pool.Pool, &run)
			})
		})
	}
	return s.database().ClaimNextRun(ctx)
}
