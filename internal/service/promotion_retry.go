package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// maxAutoMovedPromotionRetries bounds how many times in a row the server
// re-promotes on its own after delivery found the target moved (#1097). Each
// retry merges onto the target's new head and re-tests the merged result, so
// it is as safe as a fresh promote; the bound only stops a target that keeps
// moving from looping.
const maxAutoMovedPromotionRetries = 2

// promotionAutoRetryTimeout bounds one automatic retry's merge preparation
// and, for a fast-forward, its delivery. It is detached from the request or
// run whose delivery failed.
const promotionAutoRetryTimeout = 10 * time.Minute

// promoteRetry is MCP promote_retry: it re-runs a failed or interrupted
// promotion as a new promotion of the same repository, source SHA and target,
// merged onto the target's current head and re-tested exactly like promote.
// Authorization is promote's: an admin uplink directly, any other uplink with
// a matching grant. The grant of a promotion that failed before delivery is
// still unconsumed (#1097), so the original grant authorizes its retry.
func (service *API) promoteRetry(ctx context.Context, actor api.Actor, id string) (api.PromoteRetryResponse, error) {
	if strings.TrimSpace(id) == "" {
		return api.PromoteRetryResponse{}, fmt.Errorf("%w: promotion ID is required", ErrInvalidInput)
	}
	if !actor.Admin && service.promotionGrants == nil {
		return api.PromoteRetryResponse{}, fmt.Errorf("%w: promote_retry requires an admin uplink or a promotion grant", ErrForbidden)
	}
	if service.git == nil || service.promotions == nil || service.promotionRuns == nil || service.enqueues == nil || service.auditor == nil {
		return api.PromoteRetryResponse{}, fmt.Errorf("%w: promotion", ErrUnavailable)
	}
	failed, err := service.promotions.Promotion(ctx, strings.TrimSpace(id))
	if err != nil {
		return api.PromoteRetryResponse{}, err
	}
	switch failed.Status {
	case model.PromotionFailed, model.PromotionInterrupted:
	case model.PromotionPassed:
		return api.PromoteRetryResponse{}, fmt.Errorf("%w: promotion %s already delivered %s to %s", ErrInvalidInput, failed.ID, failed.SourceSHA, failed.TargetRef)
	default:
		return api.PromoteRetryResponse{}, fmt.Errorf("%w: promotion %s is still %s; wait for it with promote_status", ErrInvalidInput, failed.ID, failed.Status)
	}
	// The store repeats this inside the admission transaction; checking first
	// answers "already retried" before an authorization refusal could.
	if lineage, err := service.promotions.PromotionLineage(ctx, failed.ID); err != nil {
		return api.PromoteRetryResponse{}, err
	} else if lineage.RetriedAs != "" {
		return api.PromoteRetryResponse{}, fmt.Errorf("%w: promotion %s was already retried as %s; follow it with promote_status", ErrInvalidInput, failed.ID, lineage.RetriedAs)
	}
	retried, err := service.retryPromotion(ctx, actor, failed, model.PromotionRetryManual)
	if err != nil {
		return api.PromoteRetryResponse{}, err
	}
	return api.PromoteRetryResponse{ID: retried.ID, RetryOf: failed.ID}, nil
}

// retryPromotion admits and plans the retry of failed as actor.
func (service *API) retryPromotion(ctx context.Context, actor api.Actor, failed model.Promotion, mode string) (model.Promotion, error) {
	repository, candidate, promotion, err := service.admitPromotionRetry(ctx, actor, failed, mode)
	if err != nil {
		return model.Promotion{}, err
	}
	return service.planAdmittedPromotion(ctx, actor, repository, candidate, failed.TargetRef, promotion)
}

// admitPromotionRetry durably admits the retry of failed as actor, with its
// promotion.retry lineage, without preparing the merge.
func (service *API) admitPromotionRetry(ctx context.Context, actor api.Actor, failed model.Promotion, mode string) (model.Repository, model.Run, model.Promotion, error) {
	repository, err := service.repositoryByID(ctx, failed.RepoID)
	if err != nil {
		return model.Repository{}, model.Run{}, model.Promotion{}, err
	}
	candidate, err := service.runs.ResolveRun(ctx, repository.ID, failed.SourceSHA)
	if err != nil {
		return model.Repository{}, model.Run{}, model.Promotion{}, fmt.Errorf("resolve promotion source %s: %w", failed.SourceSHA, err)
	}
	if !sameOID(candidate.SHA, failed.SourceSHA) || candidate.Status != model.RunPassed {
		return model.Repository{}, model.Run{}, model.Promotion{}, fmt.Errorf("%w: the newest run for %s is %s (%s), not green; rerun it before retrying promotion %s",
			ErrInvalidInput, failed.SourceSHA, candidate.ID, candidate.Status, failed.ID)
	}
	promotion, err := service.admitPromotion(ctx, actor, repository, candidate, failed.TargetRef, failed.ID, mode)
	if err != nil {
		if errors.Is(err, store.ErrInvalidState) {
			return model.Repository{}, model.Run{}, model.Promotion{}, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
		}
		return model.Repository{}, model.Run{}, model.Promotion{}, err
	}
	return repository, candidate, promotion, nil
}

func (service *API) repositoryByID(ctx context.Context, id int64) (model.Repository, error) {
	if service.repositories != nil {
		return service.repositories.Repository(ctx, id)
	}
	repositories, err := service.runs.ListRepositories(ctx)
	if err != nil {
		return model.Repository{}, err
	}
	for _, repository := range repositories {
		if repository.ID == id {
			return repository, nil
		}
	}
	return model.Repository{}, fmt.Errorf("%w: repository", store.ErrNotFound)
}

// UplinkDirectory reads current uplinks by identity, so the server's own
// promotion retry re-checks the original actor's authority as it is now.
type UplinkDirectory interface {
	UplinksByIdentity(context.Context, string) ([]model.Uplink, error)
}

// currentPromotionAuthority re-resolves the authority of the uplink that
// admitted failed, as it is now. The retry continues only while an uplink with
// that identity exists. A direct (admin) promotion keeps direct authority only
// while every uplink with that identity is still an admin; a granted one acts
// as a non-admin and needs its grant, re-checked at admission. Uplinks that
// share an identity but disagree on scope are refused rather than guessed.
func (service *API) currentPromotionAuthority(ctx context.Context, failed model.Promotion) (api.Actor, error) {
	if service.uplinks == nil {
		return api.Actor{}, fmt.Errorf("%w: uplink directory", ErrUnavailable)
	}
	uplinks, err := service.uplinks.UplinksByIdentity(ctx, failed.Actor)
	if err != nil {
		return api.Actor{}, err
	}
	if len(uplinks) == 0 {
		return api.Actor{}, fmt.Errorf("%w: uplink %s that admitted promotion %s no longer exists", ErrForbidden, failed.Actor, failed.ID)
	}
	actor := api.Actor{Identity: failed.Actor, Fingerprint: uplinks[0].Fingerprint, Admin: true,
		Scope: append([]string(nil), uplinks[0].Scope...)}
	for _, uplink := range uplinks {
		actor.Admin = actor.Admin && uplink.Admin
		if !slices.Equal(uplink.Scope, uplinks[0].Scope) {
			return api.Actor{}, fmt.Errorf("%w: uplinks named %s disagree on scope", ErrForbidden, failed.Actor)
		}
	}
	if failed.GrantID == "" && !actor.Admin {
		return api.Actor{}, fmt.Errorf("%w: uplink %s promoted %s as an admin and is no longer one", ErrForbidden, failed.Actor, failed.ID)
	}
	if failed.GrantID != "" {
		// A granted promotion never gains direct authority, even if its
		// uplink has since become an admin: it stays bound to a grant.
		actor.Admin = false
	}
	return actor, nil
}

// RetryMovedPromotion is the scheduler's hook after a promotion publication
// failed because the target moved past the base it was merged onto (#1097):
// another promotion or an outside push landed first. It re-promotes the same
// tuple onto the new head, which re-tests the merged result, up to
// maxAutoMovedPromotionRetries times in a row. The original uplink's current
// authority is re-resolved (currentPromotionAuthority): a removed uplink, a
// revoked admin flag, a narrowed scope, or a revoked or expired grant stops
// it. Admission is synchronous, so promote_status on the failed promotion
// reports the retry as retried_as at once; merge preparation and any
// fast-forward delivery run in the background, holding no admission
// reservation or delivery permit of the run or request that failed.
func (service *API) RetryMovedPromotion(ctx context.Context, failed model.Promotion) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promotionAutoRetryTimeout)
	if err := service.requireMutation(ctx); err != nil {
		cancel()
		log.Printf("oberth: automatic retry of moved promotion %s skipped: %v", failed.ID, err)
		return
	}
	lineage, err := service.promotions.PromotionLineage(ctx, failed.ID)
	if err != nil {
		cancel()
		log.Printf("oberth: automatic retry of moved promotion %s skipped: %v", failed.ID, err)
		return
	}
	if lineage.Mode == model.PromotionRetryAuto && lineage.AutoAttempt >= maxAutoMovedPromotionRetries {
		cancel()
		log.Printf("oberth: promotion %s lost the moved-target race %d times in a row; not retrying again automatically (promote_retry retries it)",
			failed.ID, lineage.AutoAttempt+1)
		return
	}
	actor, err := service.currentPromotionAuthority(ctx, failed)
	if err == nil {
		var scoped *API
		if scoped, err = service.forActor(ctx, actor); err == nil {
			var repository model.Repository
			var candidate model.Run
			var retried model.Promotion
			repository, candidate, retried, err = scoped.admitPromotionRetry(ctx, actor, failed, model.PromotionRetryAuto)
			if err == nil {
				log.Printf("oberth: promotion %s found %s moved; retried as promotion %s", failed.ID, failed.TargetRef, retried.ID)
				service.background.Add(1)
				go func() {
					defer service.background.Done()
					defer cancel()
					if _, planErr := scoped.planAdmittedPromotion(ctx, actor, repository, candidate, failed.TargetRef, retried); planErr != nil {
						log.Printf("oberth: automatic retry %s of promotion %s: %v", retried.ID, failed.ID, planErr)
					}
				}()
				return
			}
		}
	}
	cancel()
	log.Printf("oberth: automatic retry of moved promotion %s failed: %v", failed.ID, err)
}

// waitBackground waits for automatic promotion retries started so far.
func (service *API) waitBackground() { service.background.Wait() }

// promotionRetriedAs names the promotion that retried a terminal, undelivered
// promotion, or "" when there is none yet.
func (service *API) promotionRetriedAs(ctx context.Context, promotion model.Promotion) (string, error) {
	if promotion.Status != model.PromotionFailed && promotion.Status != model.PromotionInterrupted {
		return "", nil
	}
	lineage, err := service.promotions.PromotionLineage(ctx, promotion.ID)
	if err != nil {
		return "", err
	}
	return lineage.RetriedAs, nil
}
