package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

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

// Admission checks current policy once. An unavailable policy fails this
// attempt; another execution requires an explicit user request.
func (scheduler *Scheduler) awaitWorkflowPolicy(ctx context.Context, repository string, runID, sha string, require mutationRequirement) (bool, error) {
	if err := require(ctx); err != nil {
		return false, err
	}
	current, err := scheduler.database().Run(ctx, runID)
	if err != nil {
		return false, err
	}
	if !current.Status.Active() || current.Cancelled() {
		return true, nil
	}
	_, err = workflowpolicy.Check(gitcache.WithBurstPolicy(ctx), scheduler.git, repository, sha)
	return false, err
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

func (scheduler *Scheduler) setPolicyWaiting(ctx context.Context, id string, waiting bool) (model.Run, error) {
	return retryDatabase(ctx, "SetRunPolicyWaiting", func(ctx context.Context) (model.Run, error) {
		return scheduler.store.SetRunPolicyWaiting(ctx, id, waiting)
	})
}
