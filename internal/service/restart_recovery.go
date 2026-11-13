package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func (scheduler *Scheduler) failRestartedRun(ctx context.Context, run model.Run, require mutationRequirement) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return scheduler.finalizeReconciledRun(ctx, run, JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: model.ServerRestartReason}, require)
}

// Retire only the uncertain-create observer's own obligation. Explicit user
// cancellation/supersession still owns its normal settlement path.
func (scheduler *Scheduler) completeRestartCreateObservation(ctx context.Context, run model.Run) error {
	current, err := scheduler.database().Run(ctx, run.ID)
	if err != nil {
		return err
	}
	if !isPendingCreate(current) {
		return nil
	}
	if current.Status != model.RunRunning {
		return errors.New("service: run changed during restart adoption")
	}
	pending, err := scheduler.database().PendingRunCancellations(ctx)
	if err != nil {
		return err
	}
	for _, observation := range pending {
		if observation.RunID == run.ID {
			if observation.Reason != "owner_restart" || observation.SupersededBy != "" {
				return errors.New("service: explicit cancellation changed during restart adoption")
			}
			return scheduler.database().CompleteObservedRunCancellation(ctx, observation)
		}
	}
	return nil
}

func (scheduler *Scheduler) registerRestartCreate(run model.Run) {
	scheduler.registerCreateCleanup(run, isPendingCreate(run))
	scheduler.createMu.Lock()
	scheduler.createPending[run.ID].restarting = true
	scheduler.createMu.Unlock()
}

// Adopt verifies the canonical Workflow's immutable identity. NotFound before
// the existing observation boundary is provisional; after it, it proves the
// old POST did not leave a Workflow. This path never invokes Delete/SettleCreate.
func (scheduler *Scheduler) observeRestartCreate(ctx context.Context, pending *pendingCreate, run model.Run, observation *model.RunCancellation) (bool, error) {
	adopter, ok := scheduler.jobs.(JobAdopter)
	if !ok {
		return false, errors.New("service: engine cannot observe restart create")
	}
	found, err := adopter.Adopt(ctx, run)
	if errors.Is(err, argoworkflow.ErrWorkflowCapabilityRequired) {
		err = fmt.Errorf("%w: %w", ErrAdoptionRefused, err)
	}
	if err != nil {
		if errors.Is(err, ErrAdoptionRefused) {
			if err := scheduler.completeRestartCreateObservation(ctx, run); err != nil {
				return false, err
			}
			if err := scheduler.handleAdoptionFailure(ctx, adopter, run, err, scheduler.requireMutation); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	boundary := pending.since.Add(createObservationWindow)
	if observation != nil {
		boundary = observation.CreatedAt.Add(createObservationWindow)
	}
	if !found && scheduler.createTime().Before(boundary) {
		return false, nil
	}
	if err := scheduler.completeRestartCreateObservation(ctx, run); err != nil {
		return false, err
	}
	if found {
		owner, err := scheduler.submissions.adopt(run)
		if err != nil {
			return false, err
		}
		select {
		case scheduler.recoveredWorkflows <- adoptedRun{run: run, reservation: owner}:
			return true, nil
		case <-ctx.Done():
			owner.cancel()
			return false, ctx.Err()
		}
	}
	if err := scheduler.failRestartedRun(ctx, run, scheduler.requireMutation); err != nil {
		return false, fmt.Errorf("fail absent restart create: %w", err)
	}
	cleanupCtx, cancel := boundedWorkspaceCleanupContext(ctx)
	defer cancel()
	_ = scheduler.workspaces.cleanupRunDeferred(cleanupCtx, run.ID)
	return true, nil
}
