package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

// A policy outage retains the claim in waiting, before any build or publication.
// No admission resources have been acquired at the initial policy boundary.
// The caller's cancellation leaves the durable claim for startup recovery.
func (scheduler *Scheduler) awaitTrustedRequirement(ctx context.Context, repository model.Repository, runID, sha string, require mutationRequirement) (vmrunner.Requirement, bool, error) {
	waitCtx, cancel := context.WithTimeout(ctx, scheduler.trustedPolicyWaitLimit())
	defer cancel()
	var lastCause error
	waited := false
	for {
		if err := ctx.Err(); err != nil {
			return vmrunner.Requirement{}, true, nil
		}
		if waitCtx.Err() != nil {
			return vmrunner.Requirement{}, false, policyWaitFailure(lastCause)
		}
		if err := require(ctx); err != nil {
			return vmrunner.Requirement{}, false, err
		}
		current, err := scheduler.database().Run(ctx, runID)
		if err != nil {
			return vmrunner.Requirement{}, false, err
		}
		if current.Status != model.RunRunning || current.Preempted() {
			return vmrunner.Requirement{}, true, nil
		}
		requirement, err := scheduler.trustedRequirement(waitCtx, repository, runID, sha)
		if ctx.Err() != nil {
			return requirement, true, nil
		}
		if waitCtx.Err() != nil {
			return requirement, false, policyWaitFailure(errors.Join(lastCause, err))
		}
		var unavailable *vmrunner.PolicyRefreshUnavailable
		if !errors.As(err, &unavailable) {
			if waited && err == nil {
				current, err = scheduler.setPolicyWaiting(ctx, runID, false)
				if err == nil && (current.Status != model.RunRunning || current.Preempted()) {
					return requirement, true, nil
				}
			}
			return requirement, false, err
		}
		if ctx.Err() != nil {
			return requirement, true, nil
		}
		current, err = scheduler.setPolicyWaiting(ctx, runID, true)
		if err != nil {
			return requirement, false, err
		}
		if current.Status != model.RunRunning || current.Preempted() {
			return requirement, true, nil
		}
		lastCause = unavailable.Cause
		waited = true
		scheduler.signals.NotifyRun(runID)
		log.Printf("WARN: trusted policy refresh waiting: run=%s reason=upstream-policy-refresh-unavailable", runID)
		// Suspending dispatch liveness while waiting matches upstream policy IO;
		// the mutation gate and cancellation are rechecked before the next read.
		resume := suspendDispatch(ctx)
		err = scheduler.capacityWait(waitCtx)
		resume()
		if ctx.Err() != nil {
			return requirement, true, nil
		}
		if waitCtx.Err() != nil {
			return requirement, false, policyWaitFailure(lastCause)
		}
		if err != nil {
			return requirement, false, err
		}
	}
}

func (scheduler *Scheduler) setPolicyWaiting(ctx context.Context, id string, waiting bool) (model.Run, error) {
	return retryDatabase(ctx, "SetRunPolicyWaiting", func(ctx context.Context) (model.Run, error) {
		return scheduler.store.SetRunPolicyWaiting(ctx, id, waiting)
	})
}

const maximumPolicyWait = 30 * time.Minute

func (scheduler *Scheduler) trustedPolicyWaitLimit() time.Duration {
	if scheduler.policyWaitTimeout > 0 && scheduler.policyWaitTimeout < maximumPolicyWait {
		return scheduler.policyWaitTimeout
	}
	return maximumPolicyWait
}

func policyWaitFailure(cause error) error {
	return fmt.Errorf("trusted policy refresh deadline exceeded: %w", errors.Join(context.DeadlineExceeded, cause))
}
