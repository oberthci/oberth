package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type promotionPinReleaser interface {
	ReleasePromotionPin(context.Context, string, string, string) error
}

type publicationAbandoner interface {
	AbandonPublication(context.Context, string, string, string) (model.PublicationFinalization, error)
}

func (service *API) abandonPublication(ctx context.Context, actor api.Actor, raw json.RawMessage) (api.PublicationAbandonResponse, error) {
	if !actor.Admin || actor.Scoped() {
		return api.PublicationAbandonResponse{}, refuse(ErrForbidden, "publication_abandon requires an unscoped admin uplink")
	}
	var args struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return api.PublicationAbandonResponse{}, err
	}
	args.ID, args.Reason = strings.TrimSpace(args.ID), strings.TrimSpace(args.Reason)
	if args.ID == "" || args.Reason == "" {
		return api.PublicationAbandonResponse{}, fmt.Errorf("%w: publication ID and abandon reason are required", ErrInvalidInput)
	}
	if err := store.ValidateCancelReason(args.Reason); err != nil {
		return api.PublicationAbandonResponse{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	owner, ok := service.enqueues.(publicationAbandoner)
	if !ok {
		return api.PublicationAbandonResponse{}, refuse(ErrUnavailable, "publication abandon requires the live scheduler")
	}
	final, err := owner.AbandonPublication(ctx, args.ID, actor.Identity, args.Reason)
	if err != nil {
		return api.PublicationAbandonResponse{}, err
	}
	response := api.PublicationAbandonResponse{
		Action: "publication_abandon", PublicationID: final.Publication.ID, Status: string(final.Publication.Status),
		RunID: final.Run.ID, RunStatus: string(final.Run.Status), RunPhase: final.Run.Phase, PromotionID: final.Promotion.ID,
		AbandonedBy: actor.Identity, Reason: final.Publication.Error,
		Message: "Publication abandoned; no further push will be attempted. An earlier push may already have reached upstream; inspect the remote ref before taking further action.",
	}
	if final.CleanupWarning != "" {
		response.Message += " " + final.CleanupWarning
	}
	return response, nil
}

// AbandonPublication shares the delivery permit with publication push paths. Once it
// returns successfully, even a previously queued retry sees a terminal intent.
// An already in-flight delivery settles first; abandonment never pushes or
// attempts remote read-back to turn an ambiguous push into a success.
func (scheduler *Scheduler) AbandonPublication(ctx context.Context, id, actor, reason string) (model.PublicationFinalization, error) {
	resume := suspendDispatch(ctx)
	select {
	case <-ctx.Done():
		resume()
		return model.PublicationFinalization{}, ctx.Err()
	case <-scheduler.deliveryPermit:
	}
	resume()
	defer func() { scheduler.deliveryPermit <- struct{}{} }()
	if err := scheduler.requireMutation(ctx); err != nil {
		return model.PublicationFinalization{}, err
	}
	database, ok := scheduler.store.(publicationAbandoner)
	if !ok {
		return model.PublicationFinalization{}, refuse(ErrUnavailable, "publication abandon")
	}
	final, err := database.AbandonPublication(ctx, id, actor, reason)
	if err != nil {
		return model.PublicationFinalization{}, err
	}
	forgetActiveLogIndex(scheduler.logs, final.Run.ID)
	if err := scheduler.finishPublication(ctx, model.Repository{}, final); err != nil {
		return final, err
	}
	scheduler.signalQueue()
	if final.Promotion.ID != "" {
		if git, ok := scheduler.git.(promotionPinReleaser); ok {
			// Abandonment stays committed if local cleanup fails. Use a separate,
			// bounded context so a cancelled caller cannot strand the pin after commit.
			cleanupCtx, cancel := boundedWorkspaceCleanupContext(ctx)
			defer cancel()
			if err := scheduler.releaseAbandonedPromotionPin(cleanupCtx, git, final.Publication); err != nil {
				log.Printf("publication abandoned: publication=%s promotion pin cleanup failed: %v", final.Publication.ID, err)
				final.CleanupWarning = "Warning: local promotion pin cleanup is pending; cleanup will retry on the next repository refresh."
			}
		}
	}
	return final, nil
}

func (scheduler *Scheduler) releaseAbandonedPromotionPin(ctx context.Context, git promotionPinReleaser, publication model.Publication) error {
	repository, err := scheduler.store.Repository(ctx, publication.RepoID)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	upstream, err := scheduler.store.Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return fmt.Errorf("resolve upstream: %w", err)
	}
	return git.ReleasePromotionPin(ctx, upstream.QualifiedRepo(repository.Name), publication.Ref, publication.ResultSHA)
}
