package service

import (
	"context"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"reflect"
	"slices"
	"time"
)

func (s *Service) prepareWait(ctx context.Context, a actor.Actor, q runQuery, timeout time.Duration) (func(context.Context) (apicontract.WaitResult, error), error) {
	a, err := s.allow(ctx, a, q.Action)
	if err != nil {
		return nil, err
	}
	if q.Until != "" && q.Until != "finished" && q.Until != "held" && q.Until != "any" {
		return nil, bad("until")
	}
	r, err := s.resolveRun(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (apicontract.WaitResult, error) {
		sub := s.c.Events.Subscribe(stateport.ChangeScope{Runs: []string{r.ID}, Invalidations: []string{r.Repo}})
		defer sub.Close()
		return s.wait(ctx, stateport.RunState{RunSummary: r.RunSummary, Revision: r.Revision}, waitOptions{until: q.Until, timeout: timeout, changed: sub.C, run: r, actor: a, action: q.Action})
	}, nil
}

type waitOptions struct {
	until   string
	timeout time.Duration
	changed <-chan struct{}
	run     stateport.Run
	actor   actor.Actor
	action  string
}

func (s *Service) wait(ctx context.Context, r stateport.RunState, o waitOptions) (apicontract.WaitResult, error) {
	if err := ctx.Err(); err != nil {
		return apicontract.WaitResult{}, err
	}
	timeout := o.timeout
	if timeout == 0 {
		timeout = toolTimeout(0)
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	initial := r
	initial.RunSummary = r.Clone()
	var initialHolds []model.HeldNotice
	first := true
	for {
		observed, err := s.waitSnapshot(ctx, wait, &o)
		if err != nil {
			return s.waitReadFailure(ctx, wait, &o, err)
		}
		if first {
			initialHolds = observed.holds
			first = false
		}
		if waitReady(observed.state, o.until, initial) || waitHoldsReady(o.until, observed.holds, initialHolds) {
			if err := s.authorizeWaitDelivery(ctx, &o); err != nil {
				return apicontract.WaitResult{}, err
			}
			return observed.result, nil
		}
		select {
		case <-s.ctx.Done():
			return apicontract.WaitResult{}, refuse(unavailable)
		case <-wait.Done():
			return s.waitTimeout(ctx, &o)
		case <-o.changed:
		}
	}
}

type observedWait struct {
	state  stateport.RunState
	result apicontract.WaitResult
	holds  []model.HeldNotice
}

func (s *Service) waitSnapshot(ctx, wait context.Context, o *waitOptions) (observedWait, error) {
	var out observedWait
	if err := s.authorizeWait(ctx, o); err != nil {
		return out, err
	}
	r, err := s.c.Runs.RunState(ctx, o.run.ID)
	if err != nil {
		return out, err
	}
	if r.Revision != o.run.Revision {
		o.run, err = s.c.Runs.Run(ctx, r.ID)
		if err != nil {
			return out, err
		}
	}
	o.run.RunSummary = r.RunSummary
	current, err := s.view(ctx, o.actor, o.run)
	if err != nil {
		return out, err
	}
	out.state, out.result.Run = r, &current
	out.holds, err = s.waitHolds(wait, r, o)
	return out, err
}

func (s *Service) waitTimeout(ctx context.Context, o *waitOptions) (apicontract.WaitResult, error) {
	if ctx.Err() != nil {
		return apicontract.WaitResult{}, ctx.Err()
	}
	out, err := s.waitSnapshot(ctx, ctx, o)
	if err != nil {
		return apicontract.WaitResult{}, err
	}
	if err := s.authorizeWaitDelivery(ctx, o); err != nil {
		return apicontract.WaitResult{}, err
	}
	out.result.TimedOut = true
	return out.result, nil
}

func (s *Service) authorizeWait(ctx context.Context, o *waitOptions) error {
	a, err := s.allow(ctx, o.actor, o.action)
	if err != nil {
		return err
	}
	_, err = s.resolveRepo(ctx, a, o.run.Repo)
	o.actor = a
	return err
}
func (s *Service) authorizeWaitDelivery(ctx context.Context, o *waitOptions) error {
	scope := o.actor.Scope
	if err := s.authorizeWait(ctx, o); err != nil {
		return err
	}
	if !slices.Equal(scope, o.actor.Scope) {
		return refuse(forbidden)
	}
	return nil
}

func waitReady(r stateport.RunState, until string, initial stateport.RunState) bool {
	return terminal(r.Status) || until == "any" && !reflect.DeepEqual(r, initial)
}
func (s *Service) waitHolds(ctx context.Context, r stateport.RunState, o *waitOptions) ([]model.HeldNotice, error) {
	if o.until != "held" && o.until != "any" || !o.run.Origin.AssistRequested || terminal(r.Status) {
		return nil, nil
	}
	if o.run.Origin.NativeUID() == "" {
		latest, err := s.c.Runs.Run(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		o.run = latest
	}
	rows, err := s.liveHolds(ctx, o.run)
	if err != nil {
		return nil, err
	}
	out := make([]model.HeldNotice, 0, len(rows))
	for _, h := range rows {
		if h.AvailabilityUnknown {
			continue
		}
		out = append(out, h.HeldNotice)
	}
	return out, nil
}

func (s *Service) waitReadFailure(ctx, wait context.Context, o *waitOptions, err error) (apicontract.WaitResult, error) {
	if ctx.Err() != nil {
		return apicontract.WaitResult{}, ctx.Err()
	}
	if wait.Err() != nil {
		return s.waitTimeout(ctx, o)
	}
	return apicontract.WaitResult{}, err
}

func waitHoldsReady(until string, current, initial []model.HeldNotice) bool {
	switch until {
	case "held":
		return len(current) > 0
	case "any":
		return !reflect.DeepEqual(current, initial)
	default:
		return false
	}
}
