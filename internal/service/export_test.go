package service

import (
	"context"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func (service *API) waitRun(ctx context.Context, repositoryName, selector, trigger string, requestedSeconds int, actor string) (WaitResponse, error) {
	prepared, err := service.prepareWaitRun(ctx, repositoryName, selector, trigger, requestedSeconds, actor)
	if err != nil {
		return WaitResponse{}, err
	}
	return prepared(ctx)
}

// Kept separate for status/diagnostic callers without exposing API errors.
func (scheduler *Scheduler) pendingCreateCount() int {
	scheduler.createMu.Lock()
	defer scheduler.createMu.Unlock()
	return len(scheduler.createPending)
}

func (scheduler *Scheduler) liveAt(now time.Time) error {
	if err := scheduler.dispatchProgress.CheckAt(now); err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	return nil
}

// waitBackground waits for automatic promotion retries started so far.
func (service *API) waitBackground() { service.background.Wait() }

// completePendingCancellations finishes durable supersession and owner-restart
// cleanup only after the idempotent Job deletion succeeds.
func (scheduler *Scheduler) completePendingCancellations(ctx context.Context) error {
	return scheduler.completePendingCancellationsWithGate(ctx, scheduler.requireMutation)
}

// recoverPendingPublications resolves the full durable outbox at owner startup.
// Live retry wakes are ID-specific so they cannot seize another delivery.
func (scheduler *Scheduler) recoverPendingPublications(ctx context.Context) error {
	return scheduler.recoverPendingPublicationsWithGate(ctx, scheduler.requireMutation)
}

// deriveStepPolicyName is retained for backward-compatible digest computation
// only. New code uses identityname.PerStepNameFromQualified directly.
// Issue #623, finding 3: the plan and the installer must produce the same name.
func deriveStepPolicyName(repo, step string) string {
	return identityname.PerStepNameFromQualified(repo, ".oberth/release.yaml", step)
}

// ExportComputeSecretStorePlan is the exported entry point for testing digest
// agreement between the CLI and the server. Issue #623, finding 5.
func ExportComputeSecretStorePlan(grants []store.SecretAccessGrant) SecretStorePlanResponse {
	return computeSecretStorePlan(grants)
}

// Historical step rendering fixtures need evidence recorded by older binaries.
type historicalStepStore struct {
	SchedulerStore
	database *store.Store
}

func (s historicalStepStore) PutStepResults(ctx context.Context, runID string, results []model.StepResult) error {
	for _, result := range results {
		result.RunID = runID
		if _, err := storetest.PutHistoricalStepResult(s.database, ctx, result); err != nil {
			return err
		}
	}
	return nil
}
func (scheduler *Scheduler) seedHistoricalSteps(ctx context.Context, database *store.Store, runID string, steps []model.StepResult, index runlog.Index) error {
	fixture := &Scheduler{store: historicalStepStore{scheduler.store, database}, logs: scheduler.logs}
	return fixture.persistSteps(ctx, runID, steps, index)
}

// fixtureNodeProgress supplies native row observations to API/log-selection tests.
func fixtureNodeProgress(database *store.Store, runID string, event runprogress.Event) error {
	return database.PutStepResults(context.Background(), runID, []model.StepResult{{
		NodeID: event.Stage + "." + event.Step, Stage: event.Stage, Step: event.Step, DisplayName: event.Step,
		Template: event.Stage + "." + event.Step, NodeType: "Pod", Ordinal: event.Ordinal,
		Status: model.StepStatus(event.Status), StartedAt: event.StartedAt, FinishedAt: event.FinishedAt,
	}})
}
