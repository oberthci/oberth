package app_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

// runFixture observes consumer calls; all storage and validation use the owner.
type runFixture struct {
	*store.Store
	t              *testing.T
	mu             sync.Mutex
	inputs         map[string]stateport.Acceptance
	DecideFunc     func(context.Context, service.RefUpdate) (service.Decision, error)
	BeforeAccepted func(context.Context, string) error
	BeforeAccept   func(context.Context, stateport.BatchAcceptance) error
	BeforeBind     func(context.Context, stateport.Action, nativebinding.RunBinding) error
	BeforeFail     func(context.Context, string) error
	BeforeUnbound  func(context.Context, int64, int) error
}

func (r *runFixture) Decide(ctx context.Context, u service.RefUpdate) (service.Decision, error) {
	if r.DecideFunc != nil {
		return r.DecideFunc(ctx, u)
	}
	return decision(u, ""), ctx.Err()
}
func (r *runFixture) Accept(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	return r.AcceptBatch(ctx, b)
}
func (r *runFixture) AcceptBatch(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	if r.BeforeAccept != nil {
		if err := r.BeforeAccept(ctx, b); err != nil {
			return stateport.AcceptanceResult{}, err
		}
	}
	out, err := r.Store.AcceptBatch(ctx, b)
	if err == nil && out.Fresh {
		r.mu.Lock()
		for _, a := range b.Runs {
			id := ""
			if a.Prepared != nil {
				id = a.Prepared.View().RunID
			} else if a.Rejected != nil {
				id = a.Rejected.ID
			}
			a.Request = a.Request.Clone()
			a.Actions = slices.Clone(a.Actions)
			r.inputs[id] = a
		}
		r.mu.Unlock()
	}
	return out, err
}
func (r *runFixture) AcceptedRuns(ctx context.Context, id string) ([]stateport.Run, error) {
	if r.BeforeAccepted != nil {
		if err := r.BeforeAccepted(ctx, id); err != nil {
			return nil, err
		}
	}
	return r.Store.AcceptedRuns(ctx, id)
}
func (r *runFixture) BindWorkflow(ctx context.Context, a stateport.Action, b nativebinding.RunBinding) error {
	if r.BeforeBind != nil {
		if err := r.BeforeBind(ctx, a, b); err != nil {
			return err
		}
	}
	return r.Store.BindWorkflow(ctx, a, b)
}
func (r *runFixture) FailUnbound(ctx context.Context, a stateport.Action, f stateport.UnboundFailure) error {
	if r.BeforeFail != nil {
		if err := r.BeforeFail(ctx, f.RunID); err != nil {
			return err
		}
	}
	return r.Store.FailUnbound(ctx, a, f)
}
func (r *runFixture) UnboundRuns(ctx context.Context, before int64, limit int) ([]stateport.Run, error) {
	if r.BeforeUnbound != nil {
		if err := r.BeforeUnbound(ctx, before, limit); err != nil {
			return nil, err
		}
	}
	return r.Store.UnboundRuns(ctx, before, limit)
}
func (r *runFixture) Admission(id string) (stateport.Acceptance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.inputs[id]
	a.Request = a.Request.Clone()
	a.Actions = slices.Clone(a.Actions)
	return a, ok
}
func (r *runFixture) Workflow(ctx context.Context, id string) (nativebinding.Object, bool) {
	row, err := r.Run(ctx, id)
	return nativebinding.Object{Namespace: row.Admission.Identity.Namespace, Name: row.Origin.WorkflowName, UID: row.Origin.WorkflowUID}, err == nil && row.Origin.WorkflowUID != ""
}

type submissionFailure struct{ Reason, Detail string }

func (r *runFixture) Failure(ctx context.Context, id string) (submissionFailure, bool) {
	row, err := r.Run(ctx, id)
	return submissionFailure{row.DisplayReason, row.Detail}, err == nil && row.Status == "failed"
}
func (r *runFixture) SetStop(ctx context.Context, id, mode string) {
	r.t.Helper()
	row, err := r.Run(ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	_, err = r.Cancel(ctx, stateport.Action{Actor: row.Origin.Actor}, stateport.RunCancellation{ID: id, Revision: row.Revision, Terminate: mode == "Terminate"})
	if err != nil {
		r.t.Fatal(err)
	}
}
