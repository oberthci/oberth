package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
)

// resolveTrustedTestRequirement reads the tests.yaml contract from the
// candidate and baseline, and returns the resolved requirement. This replaces
// the former requireTrustedTestPolicy at the build boundary: instead of always
// blocking when a contract exists, the scheduler uses the requirement to
// decide whether to inject a trusted-tests leaf.
func (scheduler *Scheduler) resolveTrustedTestRequirement(ctx context.Context, repository model.Repository, candidateSHA string) (vmrunner.Requirement, error) {
	upstream, err := scheduler.store.Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return vmrunner.Requirement{}, fmt.Errorf("load trusted test policy upstream: %w", err)
	}
	return vmrunner.ResolveRequirement(ctx, scheduler.git, upstream.QualifiedRepo(repository.Name), candidateSHA)
}

// requireTrustedTestPolicy is mandatory at build and publication boundaries.
// When the trusted test executor is enabled (trustedTestsEnabled config) and
// the contract is version 2, the run proceeds with a server-appended leaf.
// When disabled or version 1, the existing "protected suite executor
// required" error is returned.
func (scheduler *Scheduler) requireTrustedTestPolicy(ctx context.Context, repository model.Repository, candidateSHA string) error {
	requirement, err := scheduler.resolveTrustedTestRequirement(ctx, repository, candidateSHA)
	if err != nil {
		return err
	}
	if requirement.Contract == nil {
		return nil
	}
	// Version 2 contracts with the trusted executor enabled are handled by
	// the trusted-tests leaf injection in Build, not by blocking here.
	if requirement.Contract.Version >= 2 && scheduler.trustedTestsEnabled {
		return nil
	}
	return fmt.Errorf("%w: trusted test profile %s requires a protected suite executor and verified result evidence", ErrUnavailable, requirement.Contract.Profile)
}

// requirePublicationTestPolicy gates publication on trusted test evidence.
// When a trusted test is required, the publication gate checks for a passing
// receipt in the store. Without a passing receipt, publication is refused.
func (scheduler *Scheduler) requirePublicationTestPolicy(ctx context.Context, repository model.Repository, publication model.Publication) error {
	candidateSHA := publication.ResultSHA
	runID := publication.RunID
	if runID != "" {
		run, err := scheduler.store.Run(ctx, publication.RunID)
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

	requirement, err := scheduler.resolveTrustedTestRequirement(ctx, repository, candidateSHA)
	if err != nil {
		return err
	}
	if requirement.Contract == nil {
		return nil
	}

	// For v2 contracts with the executor enabled, check for a passing receipt.
	if requirement.Contract.Version >= 2 && scheduler.trustedTestsEnabled {
		if runID == "" {
			return fmt.Errorf("%w: trusted test profile %s requires a run with a receipt for publication", ErrUnavailable, requirement.Contract.Profile)
		}
		receipt, err := scheduler.store.TrustedTestReceipt(ctx, runID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: trusted test profile %s has no receipt for run %s; the trusted-tests leaf must complete",
					ErrUnavailable, requirement.Contract.Profile, runID)
			}
			return fmt.Errorf("read trusted test receipt for publication: %w", err)
		}
		if receipt.Verdict != vmrunner.VerdictPassed {
			return fmt.Errorf("%w: trusted test profile %s verdict is %s for run %s: %s",
				ErrUnavailable, requirement.Contract.Profile, receipt.Verdict, runID, receipt.RefusalReason)
		}
		// Verify the receipt binds the right candidate.
		if receipt.CandidateSHA != candidateSHA {
			return fmt.Errorf("%w: trusted test receipt candidate %s does not match publication candidate %s",
				ErrUnavailable, receipt.CandidateSHA, candidateSHA)
		}
		return nil
	}

	return fmt.Errorf("%w: trusted test profile %s requires a protected suite executor and verified result evidence", ErrUnavailable, requirement.Contract.Profile)
}
