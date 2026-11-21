package fake

import (
	"context"

	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

// Runs forwards injected owner operations and keeps no persistence or validation state.
type Runs struct {
	Receipts func(context.Context, string) ([]stateport.Run, error)
	Stops    func(context.Context, string) ([]stateport.Obligation, error)
	Fail     func(context.Context, stateport.Action, stateport.UnboundFailure) error
	Unbound  func(context.Context, int64, int) ([]stateport.Run, error)
}

func (r Runs) AcceptedRuns(ctx context.Context, id string) ([]stateport.Run, error) {
	return r.Receipts(ctx, id)
}
func (r Runs) Obligations(ctx context.Context, id string) ([]stateport.Obligation, error) {
	return r.Stops(ctx, id)
}
func (r Runs) FailUnbound(ctx context.Context, a stateport.Action, f stateport.UnboundFailure) error {
	return r.Fail(ctx, a, f)
}
func (r Runs) UnboundRuns(ctx context.Context, before int64, limit int) ([]stateport.Run, error) {
	return r.Unbound(ctx, before, limit)
}

// Service records and decides only through fixture-provided owner callbacks.
type Service struct {
	DecideFunc func(context.Context, service.RefUpdate) (service.Decision, error)
	AcceptFunc func(context.Context, stateport.BatchAcceptance) (stateport.AcceptanceResult, error)
}

func (s Service) Decide(ctx context.Context, u service.RefUpdate) (service.Decision, error) {
	return s.DecideFunc(ctx, u)
}
func (s Service) Accept(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	return s.AcceptFunc(ctx, b)
}
