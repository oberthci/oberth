package service

import (
	"context"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"slices"
	"strconv"
	"time"
)

type holdRequest struct {
	actor              actor.Actor
	run                stateport.Run
	step, reason, kind string
	argv               []string
	timeoutS           int
}
type holdLease struct {
	run  stateport.Run
	hold model.LiveHold
}

func (s *Service) liveHolds(ctx context.Context, r stateport.Run) ([]model.LiveHold, error) {
	if !r.Origin.AssistRequested || r.Origin.WorkflowUID == "" {
		return []model.LiveHold{}, nil
	}
	return s.c.Holds.List(ctx, r)
}
func (s *Service) selectedHold(ctx context.Context, r stateport.Run, step string) (model.LiveHold, error) {
	rows, err := s.liveHolds(ctx, r)
	if err != nil {
		return model.LiveHold{}, err
	}
	var selected []model.LiveHold
	for _, h := range rows {
		if step == "" || h.Step == step {
			selected = append(selected, h)
		}
	}
	if len(selected) == 0 {
		return model.LiveHold{}, refuse(holdInactive)
	}
	if len(selected) > 1 {
		return model.LiveHold{}, bad("step")
	}
	h := selected[0]
	if h.RunID != r.ID || h.NodeID == "" || h.PodUID == "" || h.PodName == "" {
		return model.LiveHold{}, refuse(holdInactive)
	}
	return h, nil
}
func (s *Service) holdAction(ctx context.Context, q holdRequest) (holdLease, error) {
	release, err := s.acquire(ctx, "hold:"+q.run.ID)
	if err != nil {
		return holdLease{}, err
	}
	defer release()
	r, err := s.c.Runs.Run(ctx, q.run.ID)
	if err != nil {
		return holdLease{}, err
	}
	h, err := s.selectedHold(ctx, r, q.step)
	if err != nil {
		return holdLease{}, err
	}
	reason := reasonAssisted
	if q.kind == "abandon" {
		reason = reasonHoldAbandon
	}
	a := stateport.Action{Actor: q.actor.Identity, Reason: q.reason, Events: []stateport.EventWrite{s.event(r, model.EventRunUpdated, reason, "assist:"+q.kind+":"+strconv.FormatInt(r.Revision, 10))}}
	intent := stateport.HoldAction{RunID: r.ID, Step: h.Step, NodeID: h.NodeID, PodUID: h.PodUID, Kind: q.kind, Revision: r.Revision, Argv: q.argv, TimeoutS: q.timeoutS}
	if err = s.c.HoldStore.Assist(ctx, a, intent); err != nil {
		return holdLease{}, err
	}
	return holdLease{run: r, hold: h}, nil
}
func (s *Service) PrepareRunHoldList(ctx context.Context, c apicontract.Caller, q apicontract.RunHoldListArgs) (apicontract.Call[apicontract.HoldPage], error) {
	a, err := s.allow(ctx, c.Actor, "run_hold_list")
	if err != nil {
		return nil, err
	}
	r, err := s.resolveRun(ctx, a, runQuery{ID: q.ID})
	if err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (apicontract.HoldPage, error) {
		rows, err := s.liveHolds(ctx, r)
		return apicontract.HoldPage{Holds: rows}, err
	}), nil
}
func (s *Service) prepareHold(ctx context.Context, c apicontract.Caller, id, action string) (holdRequest, error) {
	a, err := s.allow(ctx, c.Actor, action)
	if err != nil {
		return holdRequest{}, err
	}
	r, err := s.resolveRun(ctx, a, runQuery{ID: id})
	return holdRequest{actor: a, run: r}, err
}
func (s *Service) PrepareRunHoldAbandon(ctx context.Context, c apicontract.Caller, q apicontract.RunHoldAbandonArgs) (apicontract.Call[apicontract.Receipt], error) {
	r, err := s.prepareHold(ctx, c, q.ID, "run_hold_abandon")
	if err != nil {
		return nil, err
	}
	r.step, r.reason, r.kind = q.Step, q.Reason, "abandon"
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		lease, err := s.holdAction(ctx, r)
		if err != nil {
			return apicontract.Receipt{}, err
		}
		err = s.c.Holds.Abandon(ctx, lease.run, lease.hold)
		return apicontract.Receipt{ID: lease.run.ID, State: reasonHoldAbandon}, err
	}), nil
}
func (s *Service) PrepareRunHoldExec(ctx context.Context, c apicontract.Caller, q apicontract.RunHoldExecArgs) (apicontract.Call[apicontract.ExecResult], error) {
	r, err := s.prepareHold(ctx, c, q.ID, "run_hold_exec")
	if err != nil {
		return nil, err
	}
	if q.TimeoutS == 0 {
		q.TimeoutS = 60
	}
	q.Argv = slices.Clone(q.Argv)
	r.step, r.reason, r.kind = q.Step, q.Reason, "exec"
	r.argv, r.timeoutS = q.Argv, q.TimeoutS
	return func(ctx context.Context) (apicontract.ExecResult, error) {
		op, cancel := context.WithTimeout(ctx, time.Duration(q.TimeoutS)*time.Second)
		defer cancel()
		lease, err := s.holdAction(op, r)
		if err != nil {
			return apicontract.ExecResult{}, err
		}
		q.ID, q.Step = lease.run.ID, lease.hold.Step
		return s.c.Holds.Exec(op, lease.run, lease.hold, q)
	}, nil
}
