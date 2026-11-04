package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type QueueCapStore interface {
	SetQueueCap(context.Context, string, int) (model.QueueCapStatus, error)
}

type deferredBranchReader interface {
	DeferredBranchReason(context.Context, int64, string, string) (string, error)
}

func (service *API) queueCapConfig(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if !actor.Admin {
		return nil, refuse(ErrForbidden, "queue cap requires an admin uplink")
	}
	if err := requireUnscoped(actor, "queue_cap_config"); err != nil {
		return nil, err
	}
	if service.queueCap == nil {
		return nil, refuse(ErrUnavailable, "queue cap")
	}
	var args struct {
		MaxNotStarted *int `json:"maxNotStarted"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	if args.MaxNotStarted == nil || *args.MaxNotStarted < 0 {
		return nil, refuse(ErrInvalidInput, "nonnegative maxNotStarted is required; use 0 to disable")
	}
	value, err := service.queueCap.SetQueueCap(ctx, actor.Identity, *args.MaxNotStarted)
	if err != nil {
		return nil, err
	}
	if service.enqueues != nil {
		service.enqueues.NotifyQueue()
	}
	service.signals.NotifyRun("")
	return value, nil
}

// drainDeferredBranches is bounded per dispatch iteration. Every insertion has
// its own atomic cap check, including arrivals racing the drain. Storage retries
// follow the scheduler's background policy; API requests retain their deadlines.
func (scheduler *Scheduler) drainDeferredBranches(ctx context.Context, require mutationRequirement) error {
	state, ok := scheduler.store.(interface {
		DrainDeferredBranch(context.Context) (model.EnqueueRunResult, error)
	})
	if !ok {
		return nil
	}
	for range 64 {
		if err := require(ctx); err != nil {
			return err
		}
		result, err := retryDatabase(ctx, "DrainDeferredBranch", state.DrainDeferredBranch)
		if err != nil {
			return fmt.Errorf("drain deferred branch: %w", err)
		}
		if result.ID == "" {
			return nil
		}
		if err := scheduler.AcceptEnqueue(ctx, result); err != nil {
			return err
		}
		scheduler.signals.NotifyRun(result.ID)
	}
	scheduler.signalQueue()
	return nil
}

func (service *API) branchWithoutRun(ctx context.Context, repository model.Repository, ref, sha string) (StatusResponse, error) {
	response := StatusResponse{Repository: repository, Repo: repository.Name, Ref: ref, SHA: sha, Status: "no-runs", Stages: map[string]string{}}
	if service.deferredBranches == nil {
		return response, nil
	}
	// The repository came from the scoped run resolver before this read. The
	// reason includes global queue numbers, so scoped uplinks get plain wording.
	reason, err := service.deferredBranches.DeferredBranchReason(ctx, repository.ID, strings.TrimPrefix(ref, "refs/heads/"), sha)
	if err != nil {
		return StatusResponse{}, err
	}
	if reason != "" {
		if service.scope != nil {
			reason = "branch run deferred: waiting for queue capacity"
		}
		response.Status = "queued"
		response.DisplayStatus = model.DisplayQueued
		response.DisplayReason = reason
	}
	return response, nil
}
