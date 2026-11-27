package fake

import (
	"context"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (m *runMemory) mutable(id string, revision int64) (stateport.Run, error) {
	r, ok := m.runs[id]
	if !ok {
		return r, runProblem(publicerr.NotFound)
	}
	if r.Revision != revision || runTerminal(r) {
		return r, runProblem(publicerr.Conflict)
	}
	return r.Clone(), nil
}

func (f *Runs) Finish(ctx context.Context, a stateport.Action, u stateport.RunUpdate) error {
	if err := fakeRunUpdate(u); err != nil {
		return err
	}
	return f.transact(ctx, a.Events, func(m *runMemory) error {
		r, err := m.mutable(u.ID, u.Revision)
		if err != nil {
			return err
		}
		r.Origin.Assisted = r.Origin.Assisted || stateport.ArmedStepMayHaveHeld(r.Admission.Plan, m.native[r.ID].Stored)
		if u.ProjectCI && (r.Origin.HasAssistance()) {
			return runProblem(publicerr.Conflict)
		}
		r.Status = u.Status
		finishRemainders(&r)
		r.Failure = u.Failure
		r.DisplayReason = u.Reason
		if u.Restart {
			r.DisplayReason = stateport.ReasonRestart
		}
		r.Detail = u.Detail
		r.Diagnostic = u.Diagnostic.Clone()
		r.NativePhase = u.NativePhase
		r.Revision++
		r.FinishedAt = new(f.clock().UTC())
		if u.Publication != nil {
			if err := m.publication(a, u.Publication.Publication, r); err != nil {
				return err
			}
		}
		m.runs[r.ID] = r.Clone()
		return nil
	})
}

func (f *Runs) Cancel(ctx context.Context, a stateport.Action, u stateport.RunCancellation) (stateport.Obligation, error) {
	var result stateport.Obligation
	if a.Actor == "" {
		return result, runProblem(publicerr.InvalidInput)
	}
	err := f.transact(ctx, a.Events, func(m *runMemory) error {
		r, err := m.mutable(u.ID, u.Revision)
		if err != nil {
			return err
		}
		old := m.obligations[u.ID+"\x00stop"]
		mode := "Stop"
		if u.Terminate || old.Revision > 0 {
			mode = "Terminate"
		}
		result = stateport.Obligation{RunID: u.ID, Kind: "stop", Value: mode, Actor: a.Actor, Revision: old.Revision + 1}
		r.Revision++
		m.runs[r.ID] = r
		m.obligations[u.ID+"\x00stop"] = result
		m.record(a.Actor, "run.cancel", u.ID, a.Reason)
		return nil
	})
	if err != nil {
		return stateport.Obligation{}, err
	}
	return result, nil
}

func (f *Runs) Obligations(ctx context.Context, id string) ([]stateport.Obligation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows := []stateport.Obligation{}
	for _, o := range f.data.obligations {
		if o.RunID == id {
			rows = append(rows, o)
		}
	}
	return rows, nil
}

// AdvanceNative commits a native observation without changing the run-control revision.
func (f *Runs) AdvanceNative(ctx context.Context, id string) error {
	return f.transact(ctx, nil, func(m *runMemory) error {
		r, ok := m.runs[id]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		if !r.HasAdmission {
			return runProblem(publicerr.InvalidTransition)
		}
		r.EvidenceComplete = false
		m.runs[id] = r
		return nil
	}, stateport.Change{Run: id})
}

func fakeRunUpdate(u stateport.RunUpdate) error {
	if !u.Status.Valid() || u.Status == stepmodel.RunRunning || u.Status == stepmodel.RunQueued {
		return runProblem(publicerr.InvalidInput)
	}
	if !nativeVerdictValid(u) {
		return runProblem(publicerr.InvalidInput)
	}
	if u.Publication != nil && u.Publication.Publication.RunID != u.ID {
		return runProblem(publicerr.InvalidInput)
	}
	if u.Failure != nil && (!u.Failure.Class.Valid() || !u.Failure.Retry.Action.Valid()) {
		return runProblem(publicerr.InvalidInput)
	}
	return nil
}

func nativeVerdictValid(u stateport.RunUpdate) bool {
	return (u.Reason == "" || publicerr.CheckMessage(u.Reason, 40) == nil) && (u.NativePhase == nil || u.NativePhase.Valid())
}
