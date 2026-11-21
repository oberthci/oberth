package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/stateport"
)

// The embedded name leaves the native Runs query method visible.
type nativeStore = Runs

// MovingRuns adds service-owned terminal facts to the native owner's memory store.
// Native methods and validation remain on Runs; both share one backing state.
type MovingRuns struct{ *nativeStore }

func WithMoving(r *Runs) *MovingRuns { return &MovingRuns{nativeStore: r} }

// Finish stages the native verdict and the service's projection/promotion together.
func (f *MovingRuns) Finish(ctx context.Context, a stateport.Action, u stateport.RunUpdate) error {
	return f.movingChange(ctx, "Finish", a.Events, func(m *runMemory) error {
		staged := NewRuns(f.clock)
		staged.data = *m
		nativeAction := a
		nativeAction.Events = nil
		if err := staged.Finish(ctx, nativeAction, u); err != nil {
			return err
		}
		*m = staged.data
		if u.Promotion != nil {
			if err := m.finishPromotion(a, *u.Promotion); err != nil {
				return err
			}
		}
		if u.ProjectCI {
			return m.projectCI(m.runs[u.ID], f.clock())
		}
		return nil
	})
}
func (f *MovingRuns) Cancel(ctx context.Context, a stateport.Action, u stateport.RunCancellation) (stateport.Obligation, error) {
	if err := f.before(ctx, "Cancel", true); err != nil {
		return stateport.Obligation{}, err
	}
	return f.nativeStore.Cancel(ctx, a, u)
}
