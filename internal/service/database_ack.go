package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// These transactions return their exact candidate on commit failure. Resolve
// that candidate before retrying: a lost acknowledgement cannot orphan a claim,
// enqueue a second successor, or append a second publication intent.
func (database schedulerDatabase) ClaimNextRun(ctx context.Context) (model.Run, error) {
	var candidate model.Run
	return retryDatabase(ctx, "ClaimNextRun", func(ctx context.Context) (model.Run, error) {
		if candidate.ID != "" {
			current, err := database.SchedulerStore.Run(ctx, candidate.ID)
			if err != nil {
				return current, err
			}
			if current.Status == model.RunRunning {
				return current, nil
			}
			candidate = model.Run{}
		}
		var err error
		candidate, err = database.SchedulerStore.ClaimNextRun(ctx)
		return candidate, err
	})
}

func (database schedulerDatabase) RequeueStrandedRun(ctx context.Context, id string) (model.Run, error) {
	var candidate model.Run
	return retryDatabase(ctx, "RequeueStrandedRun", func(ctx context.Context) (model.Run, error) {
		if candidate.ID != "" {
			current, err := database.SchedulerStore.Run(ctx, candidate.ID)
			if err == nil {
				return current, nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return current, err
			}
		}
		var err error
		candidate, err = database.SchedulerStore.RequeueStrandedRun(ctx, id)
		return candidate, err
	})
}

func (database schedulerDatabase) BeginPublication(ctx context.Context, spec model.PublicationSpec) (model.Publication, error) {
	var candidate model.Publication
	return retryDatabase(ctx, "BeginPublication", func(ctx context.Context) (model.Publication, error) {
		if candidate.ID != "" {
			current, err := database.SchedulerStore.Publication(ctx, candidate.ID)
			if err == nil {
				return current, nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return current, err
			}
		}
		var err error
		candidate, err = database.SchedulerStore.BeginPublication(ctx, spec)
		return candidate, err
	})
}

func (database schedulerDatabase) FinalizePublication(ctx context.Context, id string, status model.PublicationStatus, failure string) (model.PublicationFinalization, error) {
	retried := false
	return retryDatabase(ctx, "FinalizePublication", func(ctx context.Context) (model.PublicationFinalization, error) {
		if retried {
			current, err := database.SchedulerStore.Publication(ctx, id)
			if err != nil {
				return model.PublicationFinalization{}, err
			}
			if current.Status.Terminal() {
				if current.Status != status || (status == model.PublicationFailed && current.Error != failure) {
					return model.PublicationFinalization{}, fmt.Errorf("%w: publication completion differs from pending result", store.ErrInvalidState)
				}
				result := model.PublicationFinalization{Publication: current}
				if current.RunID != "" {
					result.Run, err = database.SchedulerStore.Run(ctx, current.RunID)
					if err != nil {
						return result, err
					}
				}
				if current.PromotionID != "" {
					result.Promotion, err = database.SchedulerStore.Promotion(ctx, current.PromotionID)
				}
				return result, err
			}
		}
		retried = true
		return database.SchedulerStore.FinalizePublication(ctx, id, status, failure)
	})
}

// Run transitions stay strict for direct callers. Only a retry after a
// transient failure may accept the already-recorded state as its lost ack.
func (database schedulerDatabase) runTransition(ctx context.Context, operation, id string, applied func(model.Run) bool, transition func(context.Context, string) (model.Run, error)) (model.Run, error) {
	retried := false
	return retryDatabase(ctx, operation, func(ctx context.Context) (model.Run, error) {
		if retried {
			current, err := database.SchedulerStore.Run(ctx, id)
			if err != nil {
				return current, err
			}
			if applied(current) {
				return current, nil
			}
		}
		retried = true
		return transition(ctx, id)
	})
}
func (database schedulerDatabase) MarkRunAdmitted(ctx context.Context, id string) (model.Run, error) {
	return database.runTransition(ctx, "MarkRunAdmitted", id, func(run model.Run) bool { return run.Status == model.RunRunning && run.StartedAt != nil }, database.SchedulerStore.MarkRunAdmitted)
}
func (database schedulerDatabase) MarkRunSubmitted(ctx context.Context, id string) (model.Run, error) {
	return database.runTransition(ctx, "MarkRunSubmitted", id, func(run model.Run) bool {
		return run.Status == model.RunRunning && run.Phase == "running" && run.JobName != ""
	}, database.SchedulerStore.MarkRunSubmitted)
}
func (database schedulerDatabase) MarkRunParked(ctx context.Context, id string) (model.Run, error) {
	return database.runTransition(ctx, "MarkRunParked", id, func(run model.Run) bool {
		return run.Status == model.RunRunning && run.StartedAt == nil && run.JobName == ""
	}, database.SchedulerStore.MarkRunParked)
}
