package service

import (
	"context"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"reflect"
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
	changed := s.watch()
	r, err := s.resolveRun(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (apicontract.WaitResult, error) {
		return s.wait(ctx, stateport.RunState{RunSummary: summary(r.RunSummary), Revision: r.Revision}, waitOptions{until: q.Until, timeout: timeout, changed: changed, run: r})
	}, nil
}

type waitOptions struct {
	until   string
	timeout time.Duration
	changed <-chan struct{}
	run     stateport.Run
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
		current := summary(r.RunSummary)
		result := apicontract.WaitResult{Run: &current}
		holds, err := s.waitHolds(wait, r, &o)
		if err != nil {
			return waitReadFailure(ctx, wait, result, err)
		}
		if first {
			initialHolds = holds
			first = false
		}
		if waitReady(r, o.until, initial) || waitHoldsReady(o.until, holds, initialHolds) {
			return result, nil
		}
		select {
		case <-s.ctx.Done():
			return result, refuse(unavailable)
		case <-wait.Done():
			return waitReadFailure(ctx, wait, result, wait.Err())
		case <-o.changed:
			o.changed = s.watch()
			next, err := s.c.Runs.RunState(wait, r.ID)
			if err != nil {
				return waitReadFailure(ctx, wait, result, err)
			}
			r = next
		}
	}
}
func waitReady(r stateport.RunState, until string, initial stateport.RunState) bool {
	return terminal(r.Status) || until == "any" && !reflect.DeepEqual(r, initial)
}
func (s *Service) waitHolds(ctx context.Context, r stateport.RunState, o *waitOptions) ([]model.HeldNotice, error) {
	if o.until != "held" && o.until != "any" || !o.run.Origin.AssistRequested || terminal(r.Status) {
		return nil, nil
	}
	if o.run.Origin.WorkflowUID == "" {
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
func waitReadFailure(ctx, wait context.Context, result apicontract.WaitResult, err error) (apicontract.WaitResult, error) {
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if wait.Err() != nil {
		result.TimedOut = true
		return result, nil
	}
	return result, err
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
