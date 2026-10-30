package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func (service *API) retryRun(ctx context.Context, actor api.Actor, id string) (model.Run, error) {
	if strings.TrimSpace(id) == "" {
		return model.Run{}, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	if service.history == nil || service.runRetries == nil || service.enqueues == nil {
		return model.Run{}, refuse(ErrUnavailable, "run retry")
	}
	// The actor-scoped history resolves the ID before the writer sees it. The
	// scoped writer repeats this guard so future callers cannot bypass it.
	previous, err := service.resolveReadRunID(ctx, id)
	if err != nil {
		return model.Run{}, err
	}
	if previous.Release || previous.RefKind == model.RefTag {
		if err := authorizeRunRerun(previous, actor.Admin); err != nil {
			return model.Run{}, err
		}
	}
	enqueued, err := service.runRetries.RetryRun(ctx, model.RunRetryRequest{
		RunID: previous.ID, Actor: actor.Identity,
		Authorize: func(current model.Run, evidence model.RunReplayEvidence) error {
			_, err := RunRerunPolicy(current, actor.Admin, false, service.releaseReplaySafe(current, evidence))
			return err
		},
	})
	if err != nil {
		return model.Run{}, err
	}
	service.signals.NotifyRun(enqueued.ID)
	if err := service.enqueues.AcceptEnqueue(ctx, enqueued); err != nil {
		return enqueued.Run, err
	}
	return enqueued.Run, nil
}
