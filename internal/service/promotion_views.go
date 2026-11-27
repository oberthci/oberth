package service

import (
	"context"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Service) visiblePromotion(ctx context.Context, a actor.Actor, id string) (model.Promotion, error) {
	if id == "" {
		return model.Promotion{}, bad("id")
	}
	p, err := s.c.Promotions.Promotion(ctx, id)
	if err != nil {
		return p, err
	}
	_, err = s.resolveRepo(ctx, a, p.Repo)
	return p, err
}
func (s *Service) PreparePromoteRetry(ctx context.Context, c apicontract.Caller, q apicontract.PromoteRetryArgs) (apicontract.Call[apicontract.PromoteResult], error) {
	a, err := s.allow(ctx, c.Actor, "promote_retry")
	if err != nil {
		return nil, err
	}
	p, err := s.visiblePromotion(ctx, a, q.ID)
	if err != nil {
		return nil, err
	}
	if p.Status != model.PromotionFailed && p.Status != model.PromotionInterrupted {
		return nil, refuse(active)
	}
	d, err := s.decidePromotion(ctx, a, apicontract.PromoteArgs{Repo: p.Repo, SHA: p.SourceSHA})
	if err != nil {
		return nil, err
	}
	options := promotionStart{id: s.c.NewID(), reason: q.Reason, predecessor: p.ID}
	return bounded(func(ctx context.Context) (apicontract.PromoteResult, error) { return s.startPromotion(ctx, d, options) }), nil
}

// successor retests a promotion whose target moved. The retest is decided and
// started first; the promotion is "interrupted / retesting" only once its
// retest exists. A retest that cannot be decided or started ends the
// promotion as failed, with the cause as detail, so the person promotes again.
func (s *Service) successor(ctx context.Context, id string) error {
	p, err := s.c.Promotions.Promotion(ctx, id)
	if err != nil {
		return err
	}
	if p.Successors >= 2 {
		return s.endPromotion(ctx, p, stateport.PromotionUpdate{Status: model.PromotionFailed, Reason: reasonPromoteAgain})
	}
	d, err := s.decidePromotion(ctx, p.Caller, apicontract.PromoteArgs{Repo: p.Repo, SHA: p.SourceSHA})
	var retest apicontract.PromoteResult
	if err == nil {
		retest, err = s.startPromotion(ctx, d, promotionStart{reason: reasonPromoteMoved, predecessor: p.ID, successors: p.Successors + 1})
	}
	if err != nil && !s.retestRunning(ctx, retest.ID) {
		// The cause is also returned, so the server's log notes it.
		return errors.Join(err, s.endPromotion(ctx, p, stateport.PromotionUpdate{Status: model.PromotionFailed, Reason: reasonRetestNotStarted, Detail: failureDetail(err)}))
	}
	return s.endPromotion(ctx, p, stateport.PromotionUpdate{Status: model.PromotionInterrupted, Reason: reasonPromoteMoved})
}

// retestRunning reports a retest that exists and has not ended, although its
// start returned an error after it was recorded.
func (s *Service) retestRunning(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	retest, err := s.c.Promotions.Promotion(ctx, id)
	return err == nil && retest.Status == model.PromotionPending
}

// endPromotion records p's outcome (status, reason, detail of u) at p's revision.
func (s *Service) endPromotion(ctx context.Context, p model.Promotion, u stateport.PromotionUpdate) error {
	u.ID, u.Revision, u.RunID = p.ID, p.Revision, p.RunID
	a := stateport.Action{Actor: p.Actor, Events: []stateport.EventWrite{s.promotionEvent(u, p.Repo)}}
	return s.c.Promotions.FinishPromotion(ctx, a, u)
}

// failureDetail is a refusal's reason and remedy, whole, or the native text of
// another error, for a record's detail.
func failureDetail(err error) string {
	var problem *publicerr.Error
	if !errors.As(err, &problem) {
		return err.Error()
	}
	detail := problem.Reason() + " " + problem.Remedy()
	if problem.Detail != "" {
		detail += "\n" + problem.Detail
	}
	return detail
}
func (s *Service) PreparePromotionList(ctx context.Context, c apicontract.Caller, q apicontract.PromotionListArgs) (apicontract.Call[apicontract.PromotionPage], error) {
	a, err := s.allow(ctx, c.Actor, "promotion_list")
	if err != nil {
		return nil, err
	}
	if q.Status != "" && !model.PromotionStatus(q.Status).Valid() {
		return nil, bad("status")
	}
	scope, err := s.repositoryScope(ctx, a)
	if err != nil {
		return nil, err
	}
	if q.Repo != "" {
		r, err := s.resolveRepo(ctx, a, q.Repo)
		if err != nil {
			return nil, err
		}
		scope = []string{r.Canonical}
	}
	f := stateport.RecordFilter{Repositories: scope, Before: q.Before, Limit: pageLimit(q.Limit, 50), Status: q.Status, SourceSHA: q.SourceSHA}
	return bounded(func(ctx context.Context) (apicontract.PromotionPage, error) {
		rows, err := s.c.Promotions.Promotions(ctx, f)
		out := apicontract.PromotionPage{Promotions: rows}
		if out.Promotions == nil {
			out.Promotions = []model.Promotion{}
		}
		if len(rows) == f.Limit {
			out.NextCursor = rows[len(rows)-1].ID
		}
		return out, err
	}), nil
}
func (s *Service) PreparePromoteStatus(ctx context.Context, c apicontract.Caller, q apicontract.PromoteStatusArgs) (apicontract.Call[apicontract.PromotionWaitResult], error) {
	a, err := s.allow(ctx, c.Actor, "promote_status")
	if err != nil {
		return nil, err
	}
	p, err := s.visiblePromotion(ctx, a, q.ID)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (apicontract.PromotionWaitResult, error) {
		return s.waitPromotion(ctx, a, p, toolTimeout(q.TimeoutS))
	}, nil
}

func (s *Service) waitPromotion(ctx context.Context, a actor.Actor, p model.Promotion, timeout time.Duration) (apicontract.PromotionWaitResult, error) {
	sub := s.c.Events.Subscribe(stateport.ChangeScope{Repositories: []string{p.Repo}})
	defer sub.Close()
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if err := s.authorizePromotion(wait, a, p); err != nil {
			return apicontract.PromotionWaitResult{}, err
		}
		result, settled, err := s.promotionOutcome(wait, p.ID)
		if err != nil {
			return apicontract.PromotionWaitResult{}, err
		}
		if settled {
			if err := s.authorizePromotion(wait, a, p); err != nil {
				return apicontract.PromotionWaitResult{}, err
			}
			return result, nil
		}
		if err := s.awaitEventChange(wait, sub.C, nil); err != nil {
			return s.promotionTimeout(ctx, a, p, err)
		}
	}
}

func (s *Service) promotionTimeout(ctx context.Context, a actor.Actor, p model.Promotion, cause error) (apicontract.PromotionWaitResult, error) {
	if ctx.Err() != nil {
		return apicontract.PromotionWaitResult{}, ctx.Err()
	}
	if !errors.Is(cause, context.DeadlineExceeded) {
		return apicontract.PromotionWaitResult{}, cause
	}
	if err := s.authorizePromotion(ctx, a, p); err != nil {
		return apicontract.PromotionWaitResult{}, err
	}
	result, _, err := s.promotionOutcome(ctx, p.ID)
	if err != nil {
		return apicontract.PromotionWaitResult{}, err
	}
	if err := s.authorizePromotion(ctx, a, p); err != nil {
		return apicontract.PromotionWaitResult{}, err
	}
	result.TimedOut = true
	return result, nil
}

func (s *Service) authorizePromotion(ctx context.Context, a actor.Actor, p model.Promotion) error {
	a, err := s.allow(ctx, a, "promote_status")
	if err != nil {
		return err
	}
	_, err = s.resolveRepo(ctx, a, p.Repo)
	return err
}

// promotionOutcome follows a promotion that the server retests on a moved
// default branch to its successor, whose outcome is the person's outcome, and
// settles a passed promotion only with its publication: a delivered
// publication is already the cache's value of the target branch. The lineage
// ends: a promotion has at most two successors.
func (s *Service) promotionOutcome(ctx context.Context, id string) (apicontract.PromotionWaitResult, bool, error) {
	for {
		current, err := s.c.Promotions.Promotion(ctx, id)
		if err != nil {
			return apicontract.PromotionWaitResult{}, false, err
		}
		result := apicontract.PromotionWaitResult{Promotion: &current}
		if current.Status == model.PromotionPassed {
			publication, err := s.c.Publications.Publication(ctx, promotionPublicationID(id))
			if err != nil {
				return apicontract.PromotionWaitResult{}, false, err
			}
			result.Publication = &publication
		}
		if targetMoved(current, result.Publication) {
			next, found, err := s.promotionSuccessor(ctx, current)
			if err != nil || !found {
				return result, false, err
			}
			id = next
			continue
		}
		pending := current.Status == model.PromotionPending || result.Publication != nil && result.Publication.Status == model.PublicationPending
		return result, !pending, nil
	}
}

// targetMoved reports a promotion handed on to a retest because its target
// moved: interrupted for the retest, or passed with its publication abandoned
// because the ref moved (its retest, or its failure, is being recorded).
func targetMoved(p model.Promotion, publication *model.Publication) bool {
	if p.Status == model.PromotionInterrupted {
		return p.Reason == reasonPromoteMoved
	}
	return publication != nil && publication.Status == model.PublicationAbandoned && publication.Reason == reasonPubMoved
}

// promotionSuccessor finds the retest the server started for p, if any yet.
func (s *Service) promotionSuccessor(ctx context.Context, p model.Promotion) (string, bool, error) {
	rows, err := s.c.Promotions.Promotions(ctx, stateport.RecordFilter{Repositories: []string{p.Repo}, SourceSHA: p.SourceSHA, Limit: stateport.RunPageLimit})
	for _, row := range rows {
		if row.PredecessorID == p.ID {
			return row.ID, true, err
		}
	}
	return "", false, err
}
