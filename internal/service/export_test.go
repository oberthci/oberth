package service

import (
	"context"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// newWeightedAdmissionForJobs converts the operator's configured concurrent-run
// ceiling into the weight budget this gate is denominated in.
//
// The two are different units and conflating them silently caps the whole
// server at one run: a budget of maxConcurrentJobs weight admits only
// floor(maxConcurrentJobs / weight) runs, which for the default M size (weight
// 2) is one run at maxConcurrentJobs=3. Scaling by the default size's weight
// makes maxConcurrentJobs runs of an undeclared-size pipeline fit exactly, so
// the configured number is the number an ordinary repository actually gets.
// A pipeline that declares a heavier size then trades concurrency for
// per-run resources proportionally, instead of collapsing the server to one.
func newWeightedAdmissionForJobs(maxConcurrentJobs int) *weightedAdmission {
	return newWeightedAdmissionForJobsWeights(maxConcurrentJobs, DefaultSizeWeights)
}

// reserve records FIFO claim order before exact-SHA checkout and static
// parsing can finish out of order.
func (gate *weightedAdmission) reserve(sequence int64) *admissionReservation {
	return gate.reserveInOrder(sequence, sequence)
}

func (gate *weightedAdmission) reserveInOrder(sequence, order int64) *admissionReservation {
	return gate.reservePriorityRun(sequence, order, model.Run{})
}

// acquire admits an ungrouped run of the given size.
func (reservation *admissionReservation) acquire(ctx context.Context, size pipeline.Size) (func(), error) {
	return reservation.acquireRequest(ctx, admissionRequest{Size: size})
}

func (service *API) waitRun(ctx context.Context, repositoryName, selector, trigger string, requestedSeconds int, actor string) (WaitResponse, error) {
	prepared, err := service.prepareWaitRun(ctx, repositoryName, selector, trigger, requestedSeconds, actor)
	if err != nil {
		return WaitResponse{}, err
	}
	return prepared(ctx)
}

// Persist the server-admitted contract before POST. Recovery must never need a
// Kubernetes read, a moving repository ref or a remote fragment to restore it.
func (scheduler *Scheduler) recordCreateAdmission(run model.Run, name string, size pipeline.Size, demand argoworkflow.VMIDemand, pods ...*argoworkflow.PodDemand) error {
	var peak *argoworkflow.PodDemand
	if len(pods) > 0 {
		peak = pods[0]
	}
	return scheduler.recordResourceAdmission(run, name, size, demand, peak, "")
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
	if err := scheduler.headProgress.CheckAt(now); err != nil {
		return fmt.Errorf("admission persistence: %w", err)
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
	return identityname.PerStepNameFromQualified(repo, step)
}

// ExportComputeSecretStorePlan is the exported entry point for testing digest
// agreement between the CLI and the server. Issue #623, finding 5.
func ExportComputeSecretStorePlan(grants []store.SecretAccessGrant) SecretStorePlanResponse {
	return computeSecretStorePlan(grants)
}
