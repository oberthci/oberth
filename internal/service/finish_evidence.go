package service

import (
	"context"
	"github.com/oberthci/oberth/internal/stateport"
)

// finishingRun routes terminal intents with the same evidence rule as Finish.
// The store recomputes the mark in its finishing transaction.
func (s *Service) finishingRun(ctx context.Context, id string) (stateport.Run, error) {
	r, err := s.c.Runs.Run(ctx, id)
	if err != nil || r.Origin.Assisted {
		return r, err
	}
	for _, entry := range r.Admission.Plan.Entries {
		if !entry.HoldArmed {
			continue
		}
		nodes, err := s.c.Runs.Nodes(ctx, id)
		if err != nil {
			return r, err
		}
		r.Origin.Assisted = stateport.ArmedStepMayHaveHeld(r.Admission.Plan, nodes)
		break
	}
	return r, nil
}
