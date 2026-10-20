package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// promotionQueueMaxWait bounds how long an admitted promotion waits for the
// promotions ahead of it on the same target. Past it the promotion fails and
// can be promoted again; a restart interrupts a waiting promotion the same
// way it interrupts any promotion that never recorded its plan.
const promotionQueueMaxWait = 12 * time.Hour

// promotionQueuePoll re-reads durable promotion state while waiting, in case
// a terminal transition's signal is missed.
var promotionQueuePoll = 5 * time.Second

// pendingPromotionAhead returns the latest promotion of the same repository
// and target that was admitted before this one and is still pending, or nil.
// Admission order is the durable sequence, so of two concurrent admissions
// exactly one sees the other ahead of it.
func (service *API) pendingPromotionAhead(ctx context.Context, promotion model.Promotion) (*model.Promotion, error) {
	before := promotion.Sequence
	for {
		page, err := service.promotions.ListPromotions(ctx, model.PromotionListFilter{
			RepoID: promotion.RepoID, Status: model.PromotionPending, Before: before, Limit: 200,
		})
		if err != nil {
			return nil, err
		}
		for index := range page.Promotions {
			candidate := page.Promotions[index]
			if candidate.ID != promotion.ID && candidate.Sequence < promotion.Sequence &&
				sameTarget(candidate.TargetRef, promotion.TargetRef) {
				return &candidate, nil
			}
		}
		if page.NextBefore == 0 || page.NextBefore >= before {
			return nil, nil
		}
		before = page.NextBefore
	}
}

func sameTarget(left, right string) bool {
	normalize := func(ref string) string { return strings.TrimPrefix(strings.Trim(ref, "/"), "refs/heads/") }
	return normalize(left) == normalize(right)
}

func queuedPromotionMessage(ahead model.Promotion, target string) string {
	return fmt.Sprintf("queued behind promotion %s to %s; this promotion is prepared against %s once that one finishes",
		ahead.ID, target, target)
}

// planOrQueuePromotion prepares an admitted promotion now, or, while an
// earlier promotion to the same target of the same repository is pending,
// leaves it pending and prepares it once that one is terminal (#1021): two
// promotions merged onto the same base would otherwise race, and the second
// fail with "upstream moved" once the first publishes.
func (service *API) planOrQueuePromotion(ctx context.Context, actor api.Actor, repository model.Repository, candidate model.Run,
	target string, promotion model.Promotion) (model.Promotion, error) {
	predecessor, err := service.pendingPromotionAhead(ctx, promotion)
	if err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "check promotions queued ahead: "+err.Error(), "")
	}
	if predecessor != nil {
		service.background.Add(1)
		go func() {
			defer service.background.Done()
			service.planQueuedPromotion(context.WithoutCancel(ctx), actor, repository, candidate, target, promotion)
		}()
		promotion.Error = queuedPromotionMessage(*predecessor, target)
		return promotion, nil
	}
	return service.planAdmittedPromotion(ctx, actor, repository, candidate, target, promotion)
}

// planQueuedPromotion waits until no promotion admitted earlier to the same
// target is pending, then prepares this one against the target as it then
// stands. It runs detached from the promote call that admitted it.
func (service *API) planQueuedPromotion(ctx context.Context, actor api.Actor, repository model.Repository, candidate model.Run,
	target string, promotion model.Promotion) {
	deadline := time.Now().Add(promotionQueueMaxWait)
	for {
		// Read the signal channel before durable state, so a transition
		// between the two still wakes this wait.
		wake := service.signals.Promotion(promotion.ID)
		ahead, err := service.pendingPromotionAhead(ctx, promotion)
		if err == nil && ahead == nil {
			err = service.requireMutation(ctx)
			if err == nil {
				_, _ = service.planAdmittedPromotion(ctx, actor, repository, candidate, target, promotion)
				return
			}
		}
		if time.Now().After(deadline) {
			reason := "promotions queued ahead on " + target + " did not finish within " + promotionQueueMaxWait.String()
			if err != nil {
				reason += ": " + err.Error()
			}
			_, _ = service.failAdmittedPromotion(ctx, promotion, reason, "")
			return
		}
		timer := time.NewTimer(promotionQueuePoll)
		select {
		case <-wake:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		timer.Stop()
	}
}
