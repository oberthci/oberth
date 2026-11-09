package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// promote prune_source (#1514). The source branch is deleted in the same
// logical operation as the promotion, never by a separate blind step: only
// after the promotion passed, only when the upstream target equals the
// promotion's result, and only while the source head still equals the
// promoted SHA. Every other outcome keeps the branch and records why.

// requestPromotionPrune records the request for an admitted single-source
// promotion into the default branch.
func (service *API) requestPromotionPrune(ctx context.Context, actor api.Actor, promotion model.Promotion) string {
	if service.agentEvents == nil {
		return "not recorded: agent events are unavailable; prune the branch with branch_delete_many"
	}
	if err := service.agentEvents.store.RequestPromotionPrune(ctx, promotion.ID, actor.Identity); err != nil {
		log.Printf("WARN: prune_source request for promotion %s not recorded", promotion.ID)
		return "not recorded; the source branch is kept"
	}
	service.signals.NotifyPromotion(promotion.ID)
	return model.PromotionPrunePending
}

// checkPruneSource refuses prune_source before admission when it can never
// apply: a train, or a target that is not the default branch (only the
// default branch has the merged classification branch pruning relies on).
func (service *API) checkPruneSource(ctx context.Context, arguments promotionArguments) error {
	if arguments.Sources != nil {
		return refuse(ErrInvalidInput, "prune_source applies to a single-source promotion; prune train sources with branch_delete_many")
	}
	if service.agentEvents == nil || service.branchPrune == nil || service.branchDeletions == nil {
		return refuse(ErrUnavailable, "prune_source")
	}
	repository, _, err := service.resolvePromotionSource(ctx, arguments.Repo, arguments.SHA)
	if err != nil {
		return err
	}
	if target := strings.TrimPrefix(arguments.Branch, "refs/heads/"); repository.DefaultBranch != "" && target != repository.DefaultBranch {
		return refuse(ErrInvalidInput, "prune_source requires the default branch %q as target", repository.DefaultBranch)
	}
	return nil
}

// settlePromotionPrune decides one pending request. A transient failure
// (the upstream is unreachable) leaves it pending for the next sweep until
// agentEventPruneGiveUp, then keeps the branch.
func (service *API) settlePromotionPrune(ctx context.Context, store AgentEventStore, promotion model.Promotion, request model.PromotionPrune, now time.Time) model.PromotionPrune {
	status, reason, batchID, retry := service.decidePromotionPrune(ctx, promotion, request)
	if retry {
		if now.Sub(request.RequestedAt) < agentEventPruneGiveUp {
			return request
		}
		status, reason = model.PromotionPruneKept, "gave up after "+agentEventPruneGiveUp.String()+": "+reason
	}
	settled, err := store.FinishPromotionPrune(ctx, promotion.ID, status, reason, batchID)
	if err != nil {
		log.Printf("WARN: prune_source outcome for promotion %s not recorded", promotion.ID)
		return request
	}
	return settled
}

func (service *API) decidePromotionPrune(ctx context.Context, promotion model.Promotion, request model.PromotionPrune) (status, reason, batchID string, retry bool) {
	kept := func(format string, values ...any) (string, string, string, bool) {
		return model.PromotionPruneKept, fmt.Sprintf(format, values...), "", false
	}
	switch {
	case promotion.Status != model.PromotionPassed:
		return kept("promotion %s; source branch kept", promotion.Status)
	case promotion.SourceBranch == "" || strings.HasPrefix(promotion.SourceBranch, "refs/"):
		return kept("the source is not a branch (%q); nothing pruned", promotion.SourceBranch)
	case promotion.ResultSHA == "":
		return kept("the promotion recorded no result; source branch kept")
	case service.repositories == nil || service.branchPrune == nil || service.branchDeletions == nil:
		return kept("branch pruning is unavailable; source branch kept")
	}
	if err := service.requireMutation(ctx); err != nil {
		// Branch deletion is audited: wait for the audit gate, never bypass it.
		return model.PromotionPruneKept, "audit integrity unavailable", "", true
	}
	repository, err := service.repositories.Repository(ctx, promotion.RepoID)
	if err != nil {
		return kept("repository unavailable; source branch kept")
	}
	input := service.cacheInputFor(ctx, repository)
	remote, ok := service.git.(interface {
		RemoteRef(context.Context, string, string) (string, bool, error)
	})
	if !ok {
		return kept("upstream verification is unavailable; source branch kept")
	}
	target := strings.TrimPrefix(promotion.TargetRef, "refs/heads/")
	upstream, exists, err := remote.RemoteRef(ctx, input, "refs/heads/"+target)
	if err != nil {
		return model.PromotionPruneKept, "upstream " + target + " could not be read", "", true
	}
	if !exists || !sameOID(upstream, promotion.ResultSHA) {
		return kept("upstream %s is %s, not the promotion result %s; source branch kept", target, shortSHA(upstream), shortSHA(promotion.ResultSHA))
	}
	// Guard (#1296): only the promotion's own source branch, never the
	// default branch, the target or a reserved (protected) branch. The plan
	// below refuses them too; this keeps the rule independent of it.
	if promotion.SourceBranch == repository.DefaultBranch || promotion.SourceBranch == target {
		return kept("source branch %s is the default or target branch; never pruned", promotion.SourceBranch)
	}
	if reserved, err := service.reservedSourceBranch(ctx, input, promotion.SourceBranch); err != nil {
		return model.PromotionPruneKept, "branch inventory unavailable", "", true
	} else if reserved {
		return kept("source branch %s is reserved; never pruned by prune_source", promotion.SourceBranch)
	}
	args := branchPruneArguments{Repo: input, Branches: []struct {
		Branch      string `json:"branch"`
		ExpectedSHA string `json:"expected_sha"`
	}{{Branch: promotion.SourceBranch, ExpectedSHA: promotion.SourceSHA}}}
	_, input, plan, selected, err := service.planBranchPrune(ctx, args)
	if err != nil {
		return model.PromotionPruneKept, "prune plan failed: " + publicPruneError(err), "", true
	}
	if plan.MergedInto != "" && plan.MergedInto != target {
		return kept("target %s is not the default branch %s; source branch kept", target, plan.MergedInto)
	}
	if len(selected) != 1 {
		why := "not selected"
		if len(plan.Refused) > 0 {
			why = plan.Refused[0].Reason
		}
		return kept("source branch %s kept: %s", promotion.SourceBranch, why)
	}
	result, err := service.executeBranchPrune(ctx, request.Actor, repository, input, plan, selected, args)
	if err != nil {
		return kept("source branch %s kept: deletion failed: %s", promotion.SourceBranch, publicPruneError(err))
	}
	if result.Deleted != 1 {
		why := "skipped"
		if len(result.Skipped) > 0 {
			why = result.Skipped[0].Reason
		}
		return kept("source branch %s kept: %s", promotion.SourceBranch, why)
	}
	return model.PromotionPrunePruned, fmt.Sprintf("deleted %s at %s after %s upstream equals %s; upstream deletion queued", promotion.SourceBranch, shortSHA(promotion.SourceSHA), target, shortSHA(promotion.ResultSHA)), result.BatchID, false
}

func publicPruneError(err error) string {
	var reason interface{ MCPErrorReason() string }
	if errors.As(err, &reason) {
		return reason.MCPErrorReason()
	}
	return "internal error"
}

// reservedSourceBranch reports whether the mirror marks branch reserved.
func (service *API) reservedSourceBranch(ctx context.Context, input, branch string) (bool, error) {
	inventory, err := service.branchPrune.BranchInventory(ctx, input, "")
	if err != nil {
		return false, err
	}
	for _, entry := range inventory.Branches {
		if entry.Branch == branch {
			return entry.Reserved || entry.Default, nil
		}
	}
	return false, nil
}
