package fake

import (
	"context"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func (m *runMemory) purgeable(id string) error {
	r, ok := m.runs[id]
	if !ok {
		return runProblem(publicerr.NotFound)
	}
	if !runTerminal(r) {
		return runProblem(publicerr.Conflict)
	}
	for _, o := range m.obligations {
		if o.RunID == id && o.Value != "" {
			return runProblem(publicerr.Conflict)
		}
	}
	for _, p := range m.publications {
		if p.RunID == id && p.Status == "pending" {
			return runProblem(publicerr.Conflict)
		}
	}
	return nil
}

func (f *Runs) BeginPurge(ctx context.Context, a stateport.Action, id, receipt string) error {
	if receipt == "" || a.Actor == "" {
		return runProblem(publicerr.InvalidInput)
	}
	return f.movingChange(ctx, "BeginPurge", a.Events, func(m *runMemory) error {
		if err := m.purgeable(id); err != nil {
			return err
		}
		if p, ok := m.purges[id]; ok {
			if p.ID != receipt {
				return runProblem(publicerr.Conflict)
			}
			return noRunChange{}
		}
		m.purges[id] = stateport.PurgeReceipt{RunID: id, ID: receipt}
		m.record(a.Actor, "logs.purge.begin", id, a.Reason)
		return nil
	})
}

func (f *Runs) FinishPurge(ctx context.Context, a stateport.Action, id, receipt string) error {
	return f.movingChange(ctx, "FinishPurge", a.Events, func(m *runMemory) error {
		if err := m.purgeable(id); err != nil {
			return err
		}
		p, ok := m.purges[id]
		if !ok || p.ID != receipt {
			return runProblem(publicerr.Conflict)
		}
		p.Completed = true
		m.purges[id] = p
		m.record(a.Actor, "logs.purge.finish", id, a.Reason)
		return nil
	})
}

func (f *Runs) PurgeReceipt(ctx context.Context, id string) (stateport.PurgeReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "PurgeReceipt", false); err != nil {
		return stateport.PurgeReceipt{}, err
	}
	p, ok := f.data.purges[id]
	if !ok {
		return p, runProblem(publicerr.NotFound)
	}
	return p, nil
}
