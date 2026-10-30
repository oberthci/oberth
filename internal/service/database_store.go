package service

// This adapter is used only at scheduler storage boundaries. The original
// scheduler.store remains intact so optional capabilities are never fabricated
// or hidden. HTTP/MCP callers without a background retry scope keep their
// original deadline and error behavior.
import (
	"context"

	"github.com/oberthci/oberth/internal/model"
)

type schedulerDatabase struct{ SchedulerStore }

func (scheduler *Scheduler) database() schedulerDatabase { return schedulerDatabase{scheduler.store} }

func (database schedulerDatabase) EnqueueReceiveEvent(ctx context.Context, event model.ReceiveEventSpec, spec model.RunSpec) (model.EnqueueRunResult, error) {
	return retryDatabase(ctx, "EnqueueReceiveEvent", func(ctx context.Context) (model.EnqueueRunResult, error) {
		return database.SchedulerStore.EnqueueReceiveEvent(ctx, event, spec)
	})
}

func (database schedulerDatabase) MarkRunCreatePending(ctx context.Context, runID string, jobName string) error {
	return retryDatabaseError(ctx, "MarkRunCreatePending", func(ctx context.Context) error {
		return database.SchedulerStore.MarkRunCreatePending(ctx, runID, jobName)
	})
}

func (database schedulerDatabase) Run(ctx context.Context, id string) (model.Run, error) {
	return retryDatabase(ctx, "Run", func(ctx context.Context) (model.Run, error) { return database.SchedulerStore.Run(ctx, id) })
}

func (database schedulerDatabase) SetRunJobName(ctx context.Context, runID string, jobName string) (model.Run, error) {
	return retryDatabase(ctx, "SetRunJobName", func(ctx context.Context) (model.Run, error) {
		return database.SchedulerStore.SetRunJobName(ctx, runID, jobName)
	})
}

func (database schedulerDatabase) SetRunCredentialed(ctx context.Context, runID string) error {
	return retryDatabaseError(ctx, "SetRunCredentialed", func(ctx context.Context) error { return database.SchedulerStore.SetRunCredentialed(ctx, runID) })
}

func (database schedulerDatabase) SetRunConcurrencyGroup(ctx context.Context, runID string, group string) error {
	return retryDatabaseError(ctx, "SetRunConcurrencyGroup", func(ctx context.Context) error {
		return database.SchedulerStore.SetRunConcurrencyGroup(ctx, runID, group)
	})
}

func (database schedulerDatabase) PendingRunCancellations(ctx context.Context) ([]model.RunCancellation, error) {
	return retryDatabase(ctx, "PendingRunCancellations", func(ctx context.Context) ([]model.RunCancellation, error) {
		return database.SchedulerStore.PendingRunCancellations(ctx)
	})
}

func (database schedulerDatabase) CompleteRunCancellation(ctx context.Context, runID string, jobName string) error {
	return retryDatabaseError(ctx, "CompleteRunCancellation", func(ctx context.Context) error {
		return database.SchedulerStore.CompleteRunCancellation(ctx, runID, jobName)
	})
}

func (database schedulerDatabase) CompleteObservedRunCancellation(ctx context.Context, cancellation model.RunCancellation) error {
	return retryDatabaseError(ctx, "CompleteObservedRunCancellation", func(ctx context.Context) error {
		return database.SchedulerStore.CompleteObservedRunCancellation(ctx, cancellation)
	})
}

func (database schedulerDatabase) RearmRunCancellation(ctx context.Context, runID string, jobName string) error {
	return retryDatabaseError(ctx, "RearmRunCancellation", func(ctx context.Context) error {
		return database.SchedulerStore.RearmRunCancellation(ctx, runID, jobName)
	})
}

func (database schedulerDatabase) CompleteSupersededRunCancellationWithoutJob(ctx context.Context, runID string) error {
	return retryDatabaseError(ctx, "CompleteSupersededRunCancellationWithoutJob", func(ctx context.Context) error {
		return database.SchedulerStore.CompleteSupersededRunCancellationWithoutJob(ctx, runID)
	})
}

func (database schedulerDatabase) FinishRun(ctx context.Context, runID string, result model.RunResult) (model.Run, error) {
	return retryDatabase(ctx, "FinishRun", func(ctx context.Context) (model.Run, error) {
		return database.SchedulerStore.FinishRun(ctx, runID, result)
	})
}

func (database schedulerDatabase) PutStepResult(ctx context.Context, result model.StepResult) (model.StepResult, error) {
	return retryDatabase(ctx, "PutStepResult", func(ctx context.Context) (model.StepResult, error) {
		return database.SchedulerStore.PutStepResult(ctx, result)
	})
}

func (database schedulerDatabase) RunningRunsWithJobs(ctx context.Context) ([]model.Run, error) {
	return retryDatabase(ctx, "RunningRunsWithJobs", func(ctx context.Context) ([]model.Run, error) {
		return database.SchedulerStore.RunningRunsWithJobs(ctx)
	})
}

func (database schedulerDatabase) SetPublicationPredecessor(ctx context.Context, publicationID string, sha string, exists bool) (model.Publication, error) {
	return retryDatabase(ctx, "SetPublicationPredecessor", func(ctx context.Context) (model.Publication, error) {
		return database.SchedulerStore.SetPublicationPredecessor(ctx, publicationID, sha, exists)
	})
}

func (database schedulerDatabase) Publication(ctx context.Context, id string) (model.Publication, error) {
	return retryDatabase(ctx, "Publication", func(ctx context.Context) (model.Publication, error) {
		return database.SchedulerStore.Publication(ctx, id)
	})
}

func (database schedulerDatabase) PendingPublications(ctx context.Context) ([]model.Publication, error) {
	return retryDatabase(ctx, "PendingPublications", func(ctx context.Context) ([]model.Publication, error) {
		return database.SchedulerStore.PendingPublications(ctx)
	})
}

func (database schedulerDatabase) RetryFailedPublication(ctx context.Context, publicationID string, actor string, reason string) (model.Publication, error) {
	return retryDatabase(ctx, "RetryFailedPublication", func(ctx context.Context) (model.Publication, error) {
		return database.SchedulerStore.RetryFailedPublication(ctx, publicationID, actor, reason)
	})
}

func (database schedulerDatabase) RunWorkspaceCleanupEligible(ctx context.Context, runID string) (bool, error) {
	return retryDatabase(ctx, "RunWorkspaceCleanupEligible", func(ctx context.Context) (bool, error) {
		return database.SchedulerStore.RunWorkspaceCleanupEligible(ctx, runID)
	})
}

func (database schedulerDatabase) PromotionWorkspaceCleanupEligible(ctx context.Context, promotionID string) (bool, error) {
	return retryDatabase(ctx, "PromotionWorkspaceCleanupEligible", func(ctx context.Context) (bool, error) {
		return database.SchedulerStore.PromotionWorkspaceCleanupEligible(ctx, promotionID)
	})
}

func (database schedulerDatabase) RunWorkspaceCleanupCandidates(ctx context.Context, afterID string, limit int) ([]string, error) {
	return retryDatabase(ctx, "RunWorkspaceCleanupCandidates", func(ctx context.Context) ([]string, error) {
		return database.SchedulerStore.RunWorkspaceCleanupCandidates(ctx, afterID, limit)
	})
}

func (database schedulerDatabase) PromotionWorkspaceCleanupCandidates(ctx context.Context, afterID string, limit int) ([]string, error) {
	return retryDatabase(ctx, "PromotionWorkspaceCleanupCandidates", func(ctx context.Context) ([]string, error) {
		return database.SchedulerStore.PromotionWorkspaceCleanupCandidates(ctx, afterID, limit)
	})
}

func (database schedulerDatabase) Repository(ctx context.Context, id int64) (model.Repository, error) {
	return retryDatabase(ctx, "Repository", func(ctx context.Context) (model.Repository, error) {
		return database.SchedulerStore.Repository(ctx, id)
	})
}

func (database schedulerDatabase) Upstream(ctx context.Context, id int64) (model.Upstream, error) {
	return retryDatabase(ctx, "Upstream", func(ctx context.Context) (model.Upstream, error) { return database.SchedulerStore.Upstream(ctx, id) })
}

func (database schedulerDatabase) PromotionByRun(ctx context.Context, runID string) (model.Promotion, error) {
	return retryDatabase(ctx, "PromotionByRun", func(ctx context.Context) (model.Promotion, error) {
		return database.SchedulerStore.PromotionByRun(ctx, runID)
	})
}

func (database schedulerDatabase) Promotion(ctx context.Context, id string) (model.Promotion, error) {
	return retryDatabase(ctx, "Promotion", func(ctx context.Context) (model.Promotion, error) { return database.SchedulerStore.Promotion(ctx, id) })
}
