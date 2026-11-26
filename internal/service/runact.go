package service

import (
	"context"
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type runAction struct {
	ID, Reason, What string
	Force, Assist    bool
}

func (s *Service) prepareAction(ctx context.Context, a actor.Actor, q runAction, name string) (actor.Actor, stateport.Run, error) {
	a, err := s.allow(ctx, a, name)
	if err != nil {
		return a, stateport.Run{}, err
	}
	r, err := s.resolveRun(ctx, a, runQuery{ID: q.ID})
	return a, r, err
}
func actionPolicy(a actor.Actor, r stateport.Run) error {
	branch := r.Origin.Trigger == model.TriggerBranch && strings.HasPrefix(r.Ref, "refs/heads/") && !r.Release
	if !a.Admin && !branch {
		return refuse(forbidden)
	}
	return nil
}
func (s *Service) prepareCancel(ctx context.Context, a actor.Actor, q runAction) (func(context.Context) (apicontract.Receipt, error), error) {
	a, r, err := s.prepareAction(ctx, a, q, "run_cancel")
	if err != nil {
		return nil, err
	}
	if err := actionPolicy(a, r); err != nil {
		return nil, err
	}
	if releaseLike(r) && !q.Force {
		return nil, refuse(forceRequired)
	}
	input := stateport.RunCancellation{ID: r.ID, Revision: r.Revision, Terminate: releaseLike(r) || r.Origin.Trigger == model.TriggerPromotion}
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		o, err := s.c.Runs.Cancel(ctx, stateport.Action{Actor: a.Identity, Reason: q.Reason}, input)
		if err != nil {
			return apicontract.Receipt{}, err
		}
		effectErr := s.c.Exec.Cancel(ctx, r, o.Value == stateport.ObligationTerminate)
		latest, readErr := s.c.Runs.Run(ctx, r.ID)
		out := apicontract.Receipt{ID: r.ID, State: string(latest.Status)}
		if o.Value == stateport.ObligationStop && latest.Status == stepmodel.RunRunning {
			out.Warning = reasonEscalate
		}
		if releaseLike(r) {
			out.Warning = reasonPartial
		}
		return out, errors.Join(effectErr, readErr)
	}), nil
}

func (s *Service) prepareRerun(ctx context.Context, a actor.Actor, q runAction) (func(context.Context) (apicontract.Receipt, error), error) {
	a, r, err := s.prepareAction(ctx, a, q, "run_rerun")
	if err != nil {
		return nil, err
	}
	if !rerunnable(r) {
		return nil, refuse(active)
	}
	if r.Release && !a.Admin {
		return nil, refuse(releaseRerunAdmin)
	}
	if err := actionPolicy(a, r); err != nil {
		return nil, err
	}
	ref, err := s.c.Git.ResolveRef(ctx, r.Repo, r.Ref)
	if err != nil {
		return nil, err
	}
	if ref.ObjectSHA != r.ObjectSHA {
		return nil, refuse(moved)
	}
	if r.Origin.Resumed() {
		q.Assist = r.Origin.AssistRequested
	}
	request := StartRequest{Request: model.CIRequest{ReceiveID: "rerun:" + s.c.NewID(), Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA, Actor: a, Trigger: r.Origin.Trigger, ScheduleEntry: r.Origin.ScheduleEntry, Assist: q.Assist}, RerunOf: r.ID, Reason: q.Reason}
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		out, err := s.c.Starter.StartRun(ctx, request)
		if releaseLike(r) {
			out.Warning = reasonRelease
		}
		return out, err
	}), nil
}

func (s *Service) preparePurge(ctx context.Context, a actor.Actor, q runAction) (func(context.Context) (apicontract.Receipt, error), error) {
	a, r, err := s.prepareAction(ctx, a, q, "run_purge")
	if err != nil {
		return nil, err
	}
	if q.What != "logs" {
		return nil, bad("what")
	}
	if strings.TrimSpace(q.Reason) == "" {
		return nil, bad("reason")
	}
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		action := stateport.Action{Actor: a.Identity, Reason: q.Reason}
		receipt := "logs:" + r.ID
		if err := s.c.Runs.BeginPurge(ctx, action, r.ID, receipt); err != nil {
			return apicontract.Receipt{}, err
		}
		if err := s.c.Logs.Purge(ctx, r); err != nil {
			return apicontract.Receipt{}, err
		}
		if err := s.c.Runs.FinishPurge(ctx, action, r.ID, receipt); err != nil {
			return apicontract.Receipt{}, err
		}
		return apicontract.Receipt{ID: r.ID, State: reasonPurged}, nil
	}), nil
}

func (s *Service) prepareCheck(ctx context.Context, a actor.Actor, q CheckRequest) (func(context.Context) (apicontract.RunCheckResult, error), error) {
	a, err := s.allow(ctx, a, "run_check")
	if err != nil {
		return nil, err
	}
	repo, err := s.resolveRepo(ctx, a, q.Request.Repo)
	if err != nil {
		return nil, err
	}
	q.Request.Repo = repo.Canonical
	q.Request.Actor = a
	if q.Request.Ref == "" {
		q.Request.Ref, err = s.c.Source.DefaultBranch(ctx, repo.Canonical)
		if err != nil {
			return nil, err
		}
	}
	return bounded(func(ctx context.Context) (apicontract.RunCheckResult, error) {
		result, err := s.c.Exec.Inspect(ctx, q)
		if result.Findings == nil {
			result.Findings = []apicontract.Finding{}
		}
		if result.Checks == nil {
			result.Checks = []apicontract.Check{}
		}
		return result, err
	}), nil
}

func releaseLike(r stateport.Run) bool { return r.Release || strings.HasPrefix(r.Ref, "refs/tags/") }

func rerunnable(r stateport.Run) bool {
	return terminal(r.Status) && (r.Status != stepmodel.RunPassed || r.Origin.Resumed()) && r.Origin.Trigger != model.TriggerPromotion
}
