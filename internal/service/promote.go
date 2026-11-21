package service

import (
	"bytes"
	"context"
	"errors"
	"go.yaml.in/yaml/v3"
	"io"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type promotionStart struct {
	id                  string
	reason, predecessor string
	successors          int
}

type promotionPolicy struct {
	UplinksMayPromote    bool `yaml:"uplinks_may_promote"`
	AllowWithoutBranchCI bool `yaml:"allow_without_branch_ci"`
}
type promotionDecision struct {
	caller actor.Actor
	repo   stateport.Repository
	head   gitport.Ref
	source stateport.Run
}

func (s *Service) promotionPolicy(ctx context.Context, repo string) (gitport.Ref, promotionPolicy, error) {
	head, file, err := s.c.Source.DefaultFile(ctx, repo, ".oberth/promotion.yaml")
	var absent *gitport.AbsentError
	missing := errors.As(err, &absent) && absent.Path == ".oberth/promotion.yaml"
	if err != nil && !missing {
		var outage *gitport.UnavailableError
		if errors.As(err, &outage) {
			err = publicerr.New(publicerr.Unavailable, outage.Reason(), outage.Remedy(), err)
		}
		return head, promotionPolicy{}, err
	}
	if head.Validate() != nil || !strings.HasPrefix(head.Name, "refs/heads/") {
		return head, promotionPolicy{}, refuse(promotionPolicyInvalid)
	}
	if missing {
		return head, promotionPolicy{}, nil
	}
	var policy promotionPolicy
	decoder := yaml.NewDecoder(bytes.NewReader(file.Bytes))
	decoder.KnownFields(true)
	if decoder.Decode(&policy) != nil {
		return head, policy, refuse(promotionPolicyInvalid)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return head, policy, refuse(promotionPolicyInvalid)
	}
	return head, policy, nil
}
func (s *Service) decidePromotion(ctx context.Context, a actor.Actor, q apicontract.PromoteArgs) (promotionDecision, error) {
	d := promotionDecision{caller: a}
	r, err := s.resolveRepo(ctx, a, q.Repo)
	if err != nil {
		return d, err
	}
	d.repo = r
	if r.Archived {
		return d, archivedRepository(r.Canonical)
	}
	head, policy, err := s.promotionPolicy(ctx, r.Canonical)
	if err != nil {
		return d, err
	}
	d.head = head
	if !a.Admin && !policy.UplinksMayPromote {
		return d, uplinkPromotion(r.Canonical, q.SHA)
	}
	if q.Branch != "" && q.Branch != head.Name && q.Branch != strings.TrimPrefix(head.Name, "refs/heads/") {
		return d, bad("branch")
	}
	if q.SHA != "" && q.SHA == head.CommitSHA {
		return d, refuse(promotionCurrent)
	}
	d.source, err = s.promotionSource(ctx, r.Canonical, q.SHA, policy.AllowWithoutBranchCI)
	return d, err
}
func (s *Service) promotionSource(ctx context.Context, repo, sha string, allowUntested bool) (stateport.Run, error) {
	if !validID(sha) {
		return stateport.Run{}, bad("sha")
	}
	selected, err := s.c.Runs.RunByCommit(ctx, []string{repo}, sha)
	if err != nil {
		if !isMissing(err) || !allowUntested {
			return selected, err
		}
		if (gitport.Ref{Name: "refs/heads/source", ObjectSHA: sha, CommitSHA: sha}).Validate() != nil {
			return selected, bad("sha")
		}
		selected = stateport.Run{RunSummary: stepmodel.RunSummary{Repo: repo, ObjectSHA: sha, CommitSHA: new(sha)}}
	}
	if selected.CommitSHA == nil {
		return selected, refuse(promotionEvidence)
	}
	rows, err := s.c.Runs.Runs(ctx, stateport.RunFilter{Repositories: []string{repo}, CommitSHA: *selected.CommitSHA, Trigger: model.TriggerBranch, Limit: 1})
	if err != nil {
		return selected, err
	}
	if len(rows) == 0 {
		if allowUntested {
			selected.ID = ""
			selected.Status = ""
			return selected, nil
		}
		return selected, refuse(promotionEvidence)
	}
	source := rows[0]
	if source.Status != stepmodel.RunPassed || source.Origin.HasAssistance() {
		return source, refuse(promotionEvidence)
	}
	return source, nil
}
func (s *Service) PreparePromote(ctx context.Context, c apicontract.Caller, q apicontract.PromoteArgs) (apicontract.Call[apicontract.PromoteResult], error) {
	a, err := s.allow(ctx, c.Actor, "promote")
	if err != nil {
		return nil, err
	}
	d, err := s.decidePromotion(ctx, a, q)
	if err != nil {
		return nil, err
	}
	options := promotionStart{id: s.c.NewID(), reason: q.Reason}
	return bounded(func(ctx context.Context) (apicontract.PromoteResult, error) { return s.startPromotion(ctx, d, options) }), nil
}

// A prepared action retains its identity; replay consults the committed fact
// before preparing or starting any further work.
func (s *Service) startPromotion(ctx context.Context, d promotionDecision, options promotionStart) (apicontract.PromoteResult, error) {
	if options.id == "" {
		options.id = s.c.NewID()
	}
	release, err := s.acquire(ctx, "promotion-action:"+options.id)
	if err != nil {
		return apicontract.PromoteResult{}, err
	}
	defer release()
	p, err := s.c.Promotions.Promotion(ctx, options.id)
	if err == nil {
		return s.replayPromotion(ctx, p, options.reason)
	}
	if !isMissing(err) {
		return apicontract.PromoteResult{}, err
	}
	return s.createPromotion(ctx, d, options)
}
func (s *Service) replayPromotion(ctx context.Context, p model.Promotion, reason string) (apicontract.PromoteResult, error) {
	if p.Status == model.PromotionPending {
		return s.startPromotionRun(ctx, p, reason)
	}
	return apicontract.PromoteResult{ID: p.ID, TestedRunID: p.SourceRunID, RunID: p.RunID, CommitSHA: p.ResultSHA, State: string(p.Status)}, nil
}
func (s *Service) createPromotion(ctx context.Context, d promotionDecision, options promotionStart) (apicontract.PromoteResult, error) {
	id := options.id
	source := d.source
	prepared, err := s.c.MovingGit.PreparePromotion(ctx, gitport.PromotionRequest{Repository: d.repo.Canonical, Source: gitport.Revision{Repository: d.repo.Canonical, Commit: *source.CommitSHA}, Target: d.head, ActionReceipt: id})
	if err != nil {
		return apicontract.PromoteResult{}, err
	}
	if len(prepared.Conflicts) > 0 {
		return apicontract.PromoteResult{}, refuse(promotionConflict)
	}
	result := prepared.Result
	if result.Validate() != nil || result.Name != d.head.Name || result.ObjectSHA != result.CommitSHA {
		return apicontract.PromoteResult{}, refuse(promotionEvidence)
	}
	p := model.Promotion{ID: id, Repo: d.repo.Canonical, SourceRunID: source.ID, SourceSHA: *source.CommitSHA, SourceRef: source.Ref, TargetRef: d.head.Name, BaseSHA: d.head.ObjectSHA, ResultSHA: result.CommitSHA, PredecessorID: options.predecessor, Actor: d.caller.Identity, Caller: d.caller, Successors: options.successors, Revision: 1, Status: model.PromotionPending}
	if err := s.c.Promotions.PreparePromotion(ctx, stateport.Action{Actor: d.caller.Identity, Reason: options.reason}, p); err != nil {
		return apicontract.PromoteResult{}, err
	}
	if result.CommitSHA == *source.CommitSHA && source.Status == stepmodel.RunPassed && !source.Origin.HasAssistance() {
		intent := promotionPublication(p, source.ID)
		u := stateport.PromotionUpdate{ID: id, Revision: p.Revision, RunID: source.ID, Status: model.PromotionPassed, Reason: reasonPromotePassed, Publication: &intent}
		err := s.c.Promotions.FinishPromotion(ctx, stateport.Action{Actor: d.caller.Identity, Reason: options.reason, Events: []stateport.EventWrite{s.promotionEvent(u, p.Repo)}}, u)
		if err == nil {
			s.schedule(intent.Publication.ID, nil)
		}
		return apicontract.PromoteResult{ID: id, TestedRunID: source.ID, CommitSHA: result.CommitSHA, RunID: source.ID, State: reasonPubPending}, err
	}
	return s.startPromotionRun(ctx, p, options.reason)
}
func (s *Service) startPromotionRun(ctx context.Context, p model.Promotion, reason string) (apicontract.PromoteResult, error) {
	r := model.CIRequest{ReceiveID: "promotion:" + p.ID + ":" + p.BaseSHA, Repo: p.Repo, Ref: p.TargetRef, ObjectSHA: p.ResultSHA, CommitSHA: new(p.ResultSHA), Actor: p.Caller, Trigger: model.TriggerPromotion}
	receipt, err := s.c.Starter.StartRun(ctx, StartRequest{Request: r, PromotionID: p.ID, Reason: reason})
	out := apicontract.PromoteResult{ID: p.ID, TestedRunID: p.SourceRunID, CommitSHA: p.ResultSHA, RunID: receipt.ID, State: string(model.PromotionPending)}
	if err != nil && receipt.ID == "" {
		u := stateport.PromotionUpdate{ID: p.ID, Revision: p.Revision, Status: model.PromotionFailed, Reason: reasonPromoteStart, Detail: err.Error()}
		a := stateport.Action{Actor: p.Actor, Reason: reason, Events: []stateport.EventWrite{s.promotionEvent(u, p.Repo)}}
		write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return out, errors.Join(err, s.c.Promotions.FinishPromotion(write, a, u))
	}
	return out, err
}
