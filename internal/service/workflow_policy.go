package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func (scheduler *Scheduler) setPolicyWaiting(ctx context.Context, id string, waiting bool) (model.Run, error) {
	return retryDatabase(ctx, "SetRunPolicyWaiting", func(ctx context.Context) (model.Run, error) {
		return scheduler.store.SetRunPolicyWaiting(ctx, id, waiting)
	})
}
func (scheduler *Scheduler) trustedPolicyWaitLimit() time.Duration {
	if scheduler.policyWaitTimeout > 0 && scheduler.policyWaitTimeout < maximumPolicyWait {
		return scheduler.policyWaitTimeout
	}
	return maximumPolicyWait
}
func policyWaitFailure(cause error) error {
	return fmt.Errorf("trusted policy refresh deadline exceeded: %w", errors.Join(context.DeadlineExceeded, cause))
}

const maximumPolicyWait = 30 * time.Minute

func waitPolicyRetry(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (scheduler *Scheduler) requirePublicationTestPolicy(ctx context.Context, repository model.Repository, publication model.Publication) error {
	if publication.PromotionID != "" {
		promotion, err := scheduler.database().Promotion(ctx, publication.PromotionID)
		if err != nil {
			return err
		}
		if promotion.BranchCISkipped {
			upstream, err := scheduler.database().Upstream(ctx, repository.UpstreamID)
			if err != nil {
				return err
			}
			policy, err := readPromotionPolicy(ctx, scheduler.git, upstream.QualifiedRepo(repository.Name))
			if err != nil {
				return err
			}
			if policy.requiresBranchCI(publication.Ref) {
				return errors.New("target policy requires branch CI")
			}
		}
	}
	candidateSHA := publication.ResultSHA
	var run model.Run
	if publication.RunID != "" {
		var err error
		run, err = scheduler.database().Run(ctx, publication.RunID)
		if err != nil {
			return fmt.Errorf("load trusted test publication run: %w", err)
		}
		if run.RepoID != repository.ID {
			return fmt.Errorf("trusted test publication run does not match repository")
		}
		candidateSHA = run.TestedSHA
		if candidateSHA == "" {
			candidateSHA = run.SHA
		}
	}
	upstream, err := scheduler.database().Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return err
	}
	_, err = workflowpolicy.Check(gitcache.WithFreshPolicy(ctx), scheduler.git, upstream.QualifiedRepo(repository.Name), candidateSHA)
	return err
}

// A policy outage retains the claim in waiting, before any build or publication.
// No admission resources have been acquired at the initial policy boundary.
// The caller's cancellation leaves the durable claim for startup recovery.
func (scheduler *Scheduler) awaitWorkflowPolicy(ctx context.Context, repository string, runID, sha string, require mutationRequirement) (bool, error) {
	waitCtx, cancel := context.WithTimeout(ctx, scheduler.trustedPolicyWaitLimit())
	defer cancel()
	var lastCause error
	waited := false
	for {
		if err := ctx.Err(); err != nil {
			return true, nil
		}
		if waitCtx.Err() != nil {
			return false, policyWaitFailure(lastCause)
		}
		if err := require(ctx); err != nil {
			return false, err
		}
		current, err := scheduler.database().Run(ctx, runID)
		if err != nil {
			return false, err
		}
		if current.Status != model.RunRunning || current.Preempted() {
			return true, nil
		}
		resume := suspendDispatch(ctx)
		_, err = workflowpolicy.Check(gitcache.WithBurstPolicy(waitCtx), scheduler.git, repository, sha)
		resume()
		if ctx.Err() != nil {
			return true, nil
		}
		if waitCtx.Err() != nil {
			return false, policyWaitFailure(errors.Join(lastCause, err))
		}
		var unavailable *workflowpolicy.PolicyRefreshUnavailable
		if !errors.As(err, &unavailable) {
			if waited && err == nil {
				current, err = scheduler.setPolicyWaiting(ctx, runID, false)
				if err == nil && (current.Status != model.RunRunning || current.Preempted()) {
					return true, nil
				}
			}
			return false, err
		}
		if ctx.Err() != nil {
			return true, nil
		}
		current, err = scheduler.setPolicyWaiting(ctx, runID, true)
		if err != nil {
			return false, err
		}
		if current.Status != model.RunRunning || current.Preempted() {
			return true, nil
		}
		lastCause = unavailable.Cause
		waited = true
		scheduler.signals.NotifyRun(runID)
		log.Printf("WARN: workflow policy refresh waiting: run=%s reason=upstream-policy-refresh-unavailable", runID)
		// Suspending dispatch liveness while waiting matches upstream policy IO;
		// the mutation gate and cancellation are rechecked before the next read.
		resume = suspendDispatch(ctx)
		err = scheduler.policyRetryWait(waitCtx)
		resume()
		if ctx.Err() != nil {
			return true, nil
		}
		if waitCtx.Err() != nil {
			return false, policyWaitFailure(lastCause)
		}
		if err != nil {
			return false, err
		}
	}
}
