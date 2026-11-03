package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type promotionSourceArgument struct {
	SHA string `json:"sha"`
	Ref string `json:"ref,omitempty"`
}
type promotionArguments struct {
	model.PriorityRequest
	Repo            string                    `json:"repo"`
	SHA             string                    `json:"sha"`
	Branch          string                    `json:"branch"`
	Sources         []promotionSourceArgument `json:"sources"`
	SkipConflicting bool                      `json:"skip_conflicting"`
}
type promotionTrainGit interface {
	PreparePromotionTrain(context.Context, string, []model.PromotionSource, string, string, bool) (gitcache.PromotionTrainCandidate, error)
}
type promotionTrainPlanner interface {
	PlanPromotionTrain(context.Context, string, string, string, *model.PromotionTrain) (model.Promotion, error)
}

// checkPromotionSource applies the same policy to single sources, trains and retries.
func (service *API) checkPromotionSource(ctx context.Context, repository model.Repository, candidate model.Run, target string) (bool, bool, error) {
	if repository.ArchivedAt != nil {
		return false, false, store.ErrRepositoryArchived
	}
	if greenBranchCandidate(candidate) {
		return false, false, nil
	}
	policy, err := readPromotionPolicy(ctx, service.git, service.cacheInputFor(ctx, repository))
	if err != nil {
		return false, false, err
	}
	resolver, ok := service.git.(parkSourceResolver)
	if !ok {
		return false, false, refuse(ErrInvalidInput, "promotion source must be the exactly tested green branch commit")
	}
	ref, err := resolver.ParkSource(ctx, service.cacheInputFor(ctx, repository), candidate.SHA, policy.ParkRefPrefix)
	if err != nil {
		return false, false, err
	}
	if ref == "" || !policy.parks(ref) || ref == repository.DefaultBranch {
		return false, false, refuse(ErrInvalidInput, "promotion source must be the exactly tested green branch commit")
	}
	if policy.requiresBranchCI(target) {
		return false, false, refuse(ErrForbidden, "target policy requires branch CI")
	}
	return true, policy.AllowWithoutBranchCI, nil
}

func (service *API) promoteTrain(ctx context.Context, actor api.Actor, args promotionArguments) (model.Promotion, error) {
	if !actor.Admin && service.promotionGrants == nil {
		return model.Promotion{}, refuse(ErrForbidden, "promote requires an admin uplink or a promotion grant")
	}
	if service.git == nil || service.promotions == nil || service.promotionRuns == nil || service.enqueues == nil || service.auditor == nil {
		return model.Promotion{}, refuse(ErrUnavailable, "promotion")
	}
	if _, ok := service.git.(promotionTrainGit); !ok {
		return model.Promotion{}, refuse(ErrUnavailable, "promotion train preparation")
	}
	if _, ok := service.promotions.(promotionTrainPlanner); !ok {
		return model.Promotion{}, refuse(ErrUnavailable, "promotion train records")
	}
	args.Branch = strings.TrimPrefix(args.Branch, "refs/heads/")
	if err := gitcache.ValidateBranch(args.Branch); err != nil {
		return model.Promotion{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if len(args.Sources) == 0 || len(args.Sources) > model.MaxPromotionSources {
		return model.Promotion{}, refuse(ErrInvalidInput, "promotion train requires 1..32 sources")
	}
	for _, source := range args.Sources {
		if !validOID(source.SHA) {
			return model.Promotion{}, refuse(ErrInvalidInput, "promotion requires a full SHA for each source")
		}
	}
	repository, _, err := service.resolvePromotionSource(ctx, args.Repo, args.Sources[0].SHA)
	if err != nil {
		return model.Promotion{}, err
	}
	train := &model.PromotionTrain{SkipConflicting: args.SkipConflicting}
	for _, source := range args.Sources {
		train.Sources = append(train.Sources, model.PromotionSource{SHA: source.SHA, Ref: source.Ref})
	}
	candidate, promotion, err := service.admitPromotionTrain(ctx, actor, repository, train, args.Branch, "", "")
	if err != nil {
		return model.Promotion{}, err
	}
	return service.planOrQueuePromotion(ctx, actor, repository, candidate, args.Branch, promotion, requestedPriority(actor, args.PriorityRequest))
}

func (service *API) resolvePromotionTrainSources(ctx context.Context, repository model.Repository, train *model.PromotionTrain, target string) ([]model.Run, error) {
	if train == nil || len(train.Sources) == 0 || len(train.Sources) > model.MaxPromotionSources {
		return nil, refuse(ErrInvalidInput, "promotion train requires 1..32 sources")
	}
	seen := map[string]bool{}
	candidates := make([]model.Run, 0, len(train.Sources))
	for i, source := range train.Sources {
		key := strings.ToLower(source.SHA)
		if !validOID(source.SHA) || seen[key] {
			return nil, fmt.Errorf("%w: source %d requires a unique full SHA", ErrInvalidInput, i+1)
		}
		seen[key] = true
		_, candidate, err := service.resolvePromotionSource(ctx, service.cacheInputFor(ctx, repository), source.SHA)
		if err == nil {
			_, _, err = service.checkPromotionSource(ctx, repository, candidate, target)
		}
		if err == nil && (!sameOID(candidate.SHA, source.SHA) || source.Ref != "" && strings.TrimPrefix(source.Ref, "refs/heads/") != strings.TrimPrefix(candidate.Ref, "refs/heads/")) {
			err = refuse(ErrInvalidInput, "source SHA and ref must match the tested branch")
		}
		if err != nil {
			return nil, fmt.Errorf("promotion source %d (%s): %w", i+1, source.SHA, err)
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func (service *API) admitPromotionTrain(ctx context.Context, actor api.Actor, repository model.Repository, input *model.PromotionTrain, target, retryOf, mode string) (model.Run, model.Promotion, error) {
	candidates, err := service.resolvePromotionTrainSources(ctx, repository, input, target)
	if err != nil {
		return model.Run{}, model.Promotion{}, err
	}
	train := &model.PromotionTrain{SkipConflicting: input.SkipConflicting}
	branchCISkipped := false
	for i, candidate := range candidates {
		skipped, policyOptIn, err := service.checkPromotionSource(ctx, repository, candidate, target)
		if err != nil {
			return model.Run{}, model.Promotion{}, err
		}
		branchCISkipped = branchCISkipped || skipped
		grantID, err := service.promotionSourceGrant(ctx, actor, repository, candidate, target, retryOf, mode, i, policyOptIn)
		if err != nil {
			return model.Run{}, model.Promotion{}, fmt.Errorf("promotion source %d (%s): %w", i+1, candidate.SHA, err)
		}
		train.Sources = append(train.Sources, model.PromotionSource{SHA: candidate.SHA, Ref: candidate.Ref, GrantID: grantID})
	}
	first := train.Sources[0]
	promotion, err := service.promotions.AppendPromotion(ctx, model.PromotionSpec{RepoID: repository.ID, SourceBranch: first.Ref, SourceSHA: first.SHA, TargetRef: target, Actor: actor.Identity, GrantID: first.GrantID, RetryOf: retryOf, RetryMode: mode, Train: train, BranchCISkipped: branchCISkipped})
	return candidates[0], promotion, err
}

func (promotions scopedPromotions) PlanPromotionTrain(ctx context.Context, id, previous, result string, train *model.PromotionTrain) (model.Promotion, error) {
	if _, err := promotions.Promotion(ctx, id); err != nil {
		return model.Promotion{}, err
	}
	planner, ok := promotions.PromotionRepository.(promotionTrainPlanner)
	if !ok {
		return model.Promotion{}, refuse(ErrUnavailable, "promotion train records")
	}
	return planner.PlanPromotionTrain(ctx, id, previous, result, train)
}

func (service *API) promotionSourceGrant(ctx context.Context, actor api.Actor, repository model.Repository, candidate model.Run, target, retryOf, mode string, sourceIndex int, policyOptIn bool) (string, error) {
	var grantID string
	if retryOf != "" && mode == model.PromotionRetryAuto {
		original, err := service.promotions.Promotion(ctx, retryOf)
		if err != nil {
			return "", err
		}
		grantID = original.GrantID
		if original.Train != nil {
			if sourceIndex >= len(original.Train.Sources) {
				return "", refuse(ErrInvalidInput, "retry source is missing")
			}
			grantID = original.Train.Sources[sourceIndex].GrantID
		}
		if grantID == "" && !actor.Admin && !policyOptIn {
			return "", refuse(ErrForbidden, "automatic promotion retry lost its original authority")
		}
	} else if !actor.Admin && !policyOptIn {
		if service.promotionGrants == nil {
			return "", refuse(ErrForbidden, "promote requires an admin uplink or a promotion grant")
		}
		grant, matchErr := service.promotionGrants.MatchingPromotionGrant(ctx, repository.ID, candidate.SHA, target)
		if errors.Is(matchErr, store.ErrInvalidState) {
			return "", refuse(fmt.Errorf("%w: %s", ErrForbidden, matchErr.Error()), "matching promotion grant is held by a pending promotion; inspect promotion_list")
		}
		if errors.Is(matchErr, store.ErrNotFound) {
			return "", refuse(ErrForbidden, "no matching promotion grant; request one from an admin with promote_grant")
		}
		if matchErr != nil {
			return "", fmt.Errorf("match promotion grant: %w", matchErr)
		}
		grantID = grant.ID
	}
	return grantID, nil
}
