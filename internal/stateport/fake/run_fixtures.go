package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"slices"
)

// SeedRun supplies a stored projection to consumer fixtures without simulating admission.
func (f *Runs) SeedRun(r stateport.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data.sequence++
	if r.Sequence == 0 {
		r.Sequence = f.data.sequence
	}
	if r.Revision == 0 {
		r.Revision = 1
	}
	f.data.runs[r.ID] = r.Clone()

}
func cloneRunSteps(rows []stepmodel.RunStep) []stepmodel.RunStep {
	out := slices.Clone(rows)
	for i := range out {
		out[i] = out[i].Clone()
	}
	return out
}

// SeedSteps supplies evidence owned by native execution, including its stored counts.
func (f *Runs) SeedSteps(id string, rows []stepmodel.RunStep) {
	f.mu.Lock()
	defer f.mu.Unlock()

	r := f.data.runs[id]
	r.Steps = cloneRunSteps(rows)
	r.LogicalStepCounts, _ = stepmodel.CountSteps(rows, true)
	f.data.runs[id] = r
}

// SeedRepositories replaces the catalogue of a query fixture.
func (f *Runs) SeedRepositories(rows []stateport.Repository) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.data.repos)
	for i, r := range rows {
		if r.ID == 0 {
			r.ID = int64(i + 1)
		}
		f.data.repos[r.Canonical] = r
	}
}

// SeedEvent supplies an already committed event and signals consumer waiters.
func (f *Runs) SeedEvent(e model.AgentEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data.eventID++
	e.ID = f.data.eventID
	f.data.events = append(f.data.events, e.Clone())
	close(f.changed)
	f.changed = make(chan struct{})
}

// PruneBefore supplies a partial retained-history fixture.
func (f *Runs) PruneBefore(before int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data.events = slices.DeleteFunc(f.data.events, func(e model.AgentEvent) bool {
		if e.ID >= before {
			return false
		}
		f.data.pruned[stateport.EventScope(e)] = max(f.data.pruned[stateport.EventScope(e)], e.ID)
		return true
	})
}

// NativeProgress is a direct native fixture write, outside service's business API.
type NativeProgress struct {
	ID             string
	Revision       int64
	Status         stepmodel.RunStatus
	Reason, Detail string
	NativePhase    *stepmodel.NativePhase
}

func (f *Runs) Progress(ctx context.Context, p NativeProgress) error {
	return f.movingChange(ctx, "NativeProgress", nil, func(m *runMemory) error {
		r, err := m.mutable(p.ID, p.Revision)
		if err != nil {
			return err
		}
		r.Status = p.Status
		r.DisplayReason = p.Reason
		r.Detail = p.Detail
		r.NativePhase = p.NativePhase
		m.runs[p.ID] = r.Clone()
		return nil
	})
}

// PublicationValues returns detached prepared intents for consumer assertions.
func (f *Runs) PublicationValues() []model.Publication {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.Publication, 0, len(f.data.publications))
	for _, p := range f.data.publications {
		out = append(out, p.Clone())
	}
	slices.SortFunc(out, func(a, b model.Publication) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}
func (f *Runs) Release(id string) (model.ReleaseReceipt, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.data.publications[id]
	if !ok || p.Release == nil {
		return model.ReleaseReceipt{}, false
	}
	return *p.Release, true
}
