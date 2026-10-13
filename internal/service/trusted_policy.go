package service

import (
	"context"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

// TrustedPreflight is deliberately separate from release delivery. Execute
// finishes credential-free testing AND cleanup before the main Job is recorded.
type TrustedPreflight interface {
	Supports(vmrunner.Contract) error
	RecipeAllowed(vmrunner.KVMRecipe) error
	Execute(context.Context, JobRequest, vmrunner.Requirement, io.Writer) error
	Recover(context.Context) ([]string, error)
}

func (scheduler *Scheduler) trustedRequirement(ctx context.Context, repository model.Repository, candidateSHA string) (vmrunner.Requirement, error) {
	upstream, err := scheduler.store.Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return vmrunner.Requirement{}, fmt.Errorf("load trusted test policy upstream: %w", err)
	}
	requirement, err := vmrunner.ResolveRequirement(ctx, scheduler.git, upstream.QualifiedRepo(repository.Name), candidateSHA)
	if err != nil {
		return requirement, err
	}
	if requirement.Contract != nil {
		if scheduler.trusted == nil {
			return requirement, fmt.Errorf("%w: trusted test profile %s requires a protected suite executor and verified result evidence", ErrUnavailable, requirement.Contract.Profile)
		}
		if _, ok := scheduler.store.(vmrunner.KVMJournal); !ok {
			return requirement, fmt.Errorf("%w: trusted test journal unavailable", ErrUnavailable)
		}
		if err = scheduler.trusted.Supports(*requirement.Contract); err != nil {
			return requirement, err
		}
	}
	return requirement, nil
}

func (scheduler *Scheduler) trustedReceipt(ctx context.Context, repository model.Repository, run model.Run, requirement vmrunner.Requirement) error {
	if requirement.Contract == nil {
		return nil
	}
	journal, ok := scheduler.store.(vmrunner.KVMJournal)
	if !ok {
		return fmt.Errorf("trusted test journal unavailable")
	}
	execution, err := journal.KVMPreflight(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("read trusted test receipt: %w", err)
	}
	intent := execution.Intent
	if err := scheduler.trusted.RecipeAllowed(intent.Recipe); err != nil {
		return err
	}
	if !execution.Cleaned || execution.Rejected || execution.Refusal != nil || execution.Receipt == nil || execution.WorkflowUID == "" || execution.WorkflowUID != execution.Receipt.WorkflowUID || intent.RunID != run.ID || intent.RepoID != repository.ID || intent.CandidateSHA != requirement.CandidateSHA || intent.BaselineSHA != requirement.BaselineSHA || vmrunner.ContractDigest(&intent.Contract) != vmrunner.ContractDigest(requirement.Contract) {
		return fmt.Errorf("trusted test receipt does not match publication policy")
	}
	upstream, err := scheduler.store.Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return err
	}
	if intent.Repository != upstream.QualifiedRepo(repository.Name) {
		return fmt.Errorf("trusted test receipt repository mismatch")
	}
	return execution.Receipt.Validate(intent)
}

func (scheduler *Scheduler) requirePublicationTestPolicy(ctx context.Context, repository model.Repository, publication model.Publication) error {
	candidateSHA := publication.ResultSHA
	var run model.Run
	if publication.RunID != "" {
		var err error
		run, err = scheduler.store.Run(ctx, publication.RunID)
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
	requirement, err := scheduler.trustedRequirement(ctx, repository, candidateSHA)
	if err != nil || requirement.Contract == nil {
		return err
	}
	// Tags publish their annotated object, whose tested commit is retained on
	// the run. Branch/merge publication must publish precisely the tested commit.
	if publication.RunID != "" && ((run.Release && publication.ResultSHA != run.SHA) || (!run.Release && publication.ResultSHA != candidateSHA)) {
		return fmt.Errorf("publication differs from trusted tested candidate")
	}
	if run.ID == "" {
		lookup, ok := scheduler.store.(interface {
			LatestRunForSHA(context.Context, int64, string) (model.Run, error)
		})
		if !ok {
			return fmt.Errorf("trusted fast-forward source run unavailable")
		}
		run, err = lookup.LatestRunForSHA(ctx, repository.ID, candidateSHA)
		if err != nil || run.Status != model.RunPassed || run.TestedSHA != candidateSHA {
			return fmt.Errorf("trusted fast-forward requires a passed exact candidate run")
		}
	}
	return scheduler.trustedReceipt(ctx, repository, run, requirement)
}

func (scheduler *Scheduler) recoverTrustedPreflights(ctx context.Context) error {
	journal, ok := scheduler.store.(vmrunner.KVMJournal)
	if !ok {
		return nil
	}
	pending, err := journal.PendingKVMPreflights(ctx)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	if scheduler.trusted == nil {
		return fmt.Errorf("pending trusted preflight cleanup requires configured executor")
	}
	runs, err := scheduler.trusted.Recover(ctx)
	if err != nil {
		return err
	}
	for _, id := range runs {
		run, e := scheduler.store.Run(ctx, id)
		if e != nil {
			return e
		}
		if run.Status != model.RunRunning {
			continue
		}
		repo, e := scheduler.store.Repository(ctx, run.RepoID)
		if e != nil {
			return e
		}
		if e = scheduler.finishInfrastructureFailure(ctx, run, repo, fmt.Errorf("trusted preflight interrupted before main delivery; retry required")); e != nil {
			return e
		}
	}
	return nil
}
