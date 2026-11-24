package service

import (
	"context"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (s *Service) finishIntent(ctx context.Context, r stateport.Run, o FinishedRun) (*stateport.PublicationIntent, *stateport.PromotionUpdate, error) {
	if r.Origin.Trigger == model.TriggerPromotion {
		return s.promotionIntent(ctx, r, o)
	}
	if o.Status != stepmodel.RunPassed || r.Origin.HasAssistance() || r.Origin.RequestedFile != "" || r.Origin.Trigger == model.TriggerBranch && optionalWorkflow(r) {
		return nil, nil, nil
	}
	p := model.Publication{ID: "publication:" + r.ID, Repo: r.Repo, Ref: r.Ref, RunID: r.ID, ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA, Actor: r.Origin.Actor}
	if strings.HasPrefix(r.Ref, "refs/tags/") {
		p.PreviousSHA = new("")
	} else {
		var err error
		p.ReceivedHeads, err = s.c.Runs.ReceivedHeads(ctx, stateport.ReceivedRef{Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA, RunID: r.ID})
		if err != nil {
			return nil, nil, err
		}
	}
	return &stateport.PublicationIntent{Publication: p}, nil, nil
}

func (s *Service) promotionIntent(ctx context.Context, r stateport.Run, o FinishedRun) (*stateport.PublicationIntent, *stateport.PromotionUpdate, error) {
	p, err := s.c.Promotions.PromotionForRun(ctx, r.ID)
	if err != nil {
		return nil, nil, promotionReadFailure{err}
	}
	update := stateport.PromotionUpdate{ID: p.ID, Revision: p.Revision, RunID: r.ID, Status: model.PromotionFailed, Reason: runReason(o.Status), Detail: o.Detail}
	if o.Status != stepmodel.RunPassed || r.Origin.HasAssistance() {
		return nil, &update, nil
	}
	if r.CommitSHA == nil || *r.CommitSHA != p.ResultSHA || r.ObjectSHA != p.ResultSHA {
		return nil, nil, refuse(publicationEvidence)
	}
	update.Status = model.PromotionPassed
	update.Reason = reasonPromotePassed
	intent := promotionPublication(p, r.ID)

	return &intent, &update, nil
}

type promotionReadFailure struct{ cause error }

func (e promotionReadFailure) Error() string { return e.cause.Error() }
func (e promotionReadFailure) Unwrap() error { return e.cause }

// promotionPublicationID names the one publication of a promotion's target branch.
func promotionPublicationID(promotion string) string { return "publication:" + promotion }

func promotionPublication(p model.Promotion, runID string) stateport.PublicationIntent {
	return stateport.PublicationIntent{Publication: model.Publication{ID: promotionPublicationID(p.ID), Repo: p.Repo, Ref: p.TargetRef, RunID: runID, PromotionID: p.ID, ObjectSHA: p.ResultSHA, CommitSHA: new(p.ResultSHA), PreviousSHA: new(p.BaseSHA), Actor: p.Actor}}
}

func (s *Service) promotionEvent(p stateport.PromotionUpdate, repo string) stateport.EventWrite {
	return stateport.EventWrite{Event: model.AgentEvent{Kind: string(model.EventPromotionFinished), At: s.c.Now(), Repo: repo, RunID: p.RunID, PromotionID: p.ID, Summary: p.Reason}, Dedupe: "promotion.finished:" + p.ID + ":" + strconv.FormatInt(p.Revision, 10)}
}
