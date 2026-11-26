package service

import (
	"context"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
)

type ResumeRequest struct {
	Run        stateport.Run
	Actor      actor.Actor
	ID, Commit string
}
type resumeStarter interface {
	ResumeRun(context.Context, ResumeRequest) (apicontract.ResumeResult, error)
}
type resumeAncestry interface {
	ResumeAncestor(context.Context, string, string, string) (bool, error)
}

func resumeRefusal(reason string) error {
	return publicerr.New(publicerr.InvalidTransition, reason, "Push the fix with -o oberth.run=1 for a full unassisted run.", nil)
}

func (s *Service) PrepareRunResume(ctx context.Context, c apicontract.Caller, q apicontract.RunResumeArgs) (apicontract.Call[apicontract.ResumeResult], error) {
	if _, err := s.allow(ctx, c.Actor, "run_resume"); err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (apicontract.ResumeResult, error) { return s.resume(ctx, c.Actor, q) }), nil
}

func (s *Service) resume(ctx context.Context, a actor.Actor, q apicontract.RunResumeArgs) (apicontract.ResumeResult, error) {
	var out apicontract.ResumeResult
	a, r, err := s.prepareAction(ctx, a, runAction{ID: q.ID}, "run_resume")
	if err != nil {
		return out, err
	}
	if err = actionPolicy(a, r); err != nil {
		return out, err
	}
	if !resumableBranch(r) {
		return out, resumeRefusal("run cannot resume")
	}
	branch, err := s.c.Source.DefaultBranch(ctx, r.Repo)
	if err != nil {
		return out, err
	}
	if branch == r.Ref {
		return out, resumeRefusal("default branch cannot resume")
	}
	if err = s.resumeHead(ctx, r, q.Commit); err != nil {
		return out, err
	}
	d, err := s.Decide(ctx, RefUpdate{ID: "resume:" + s.c.NewID(), Repo: r.Repo, Ref: r.Ref, ObjectSHA: q.Commit, CommitSHA: q.Commit, Actor: a, Run: true})
	if err != nil {
		return out, err
	}
	starter, ok := s.c.Starter.(resumeStarter)
	if !ok {
		return out, refuse(unavailable)
	}
	return starter.ResumeRun(ctx, ResumeRequest{Run: r, Actor: d.Request.Actor, ID: d.Request.ReceiveID, Commit: q.Commit})
}

func (s *Service) resumeHead(ctx context.Context, r stateport.Run, commit string) error {
	if (gitport.Revision{Repository: r.Repo, Commit: commit}).Validate() != nil {
		return bad("commit")
	}
	ref, err := s.c.Git.ResolveRef(ctx, r.Repo, r.Ref)
	if err != nil {
		return err
	}
	if ref.CommitSHA != commit || ref.ObjectSHA != commit || commit == *r.CommitSHA && !r.Origin.Resumed() {
		return resumeRefusal("fix must be the new branch head")
	}
	ancestry, ok := s.c.Git.(resumeAncestry)
	if !ok {
		return refuse(unavailable)
	}
	yes, err := ancestry.ResumeAncestor(ctx, r.Repo, *r.CommitSHA, commit)
	if err != nil {
		return err
	}
	if !yes {
		return resumeRefusal("fix must descend from the failed commit")
	}
	return nil
}

func resumableBranch(r stateport.Run) bool {
	phase := r.Status == stepmodel.RunFailed || r.Status == stepmodel.RunRunning && r.Origin.AssistRequested
	return strings.HasPrefix(r.Ref, "refs/heads/") && r.Origin.ScheduleEntry == "" && phase && r.Origin.Trigger == model.TriggerBranch && !r.Release && r.CommitSHA != nil
}
