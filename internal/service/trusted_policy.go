package service

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

// requireTrustedTestPolicy is mandatory at build and publication boundaries.
// The current VM backend proves lifecycle and cleanup, but cannot attest to an
// arbitrary candidate BPF program's guest-side test result. Keep requirements
// closed until a protected suite executor supplies independently bound evidence.
func (scheduler *Scheduler) requireTrustedTestPolicy(ctx context.Context, repository model.Repository, candidateSHA string) error {
	upstream, err := scheduler.store.Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return fmt.Errorf("load trusted test policy upstream: %w", err)
	}
	requirement, err := vmrunner.ResolveRequirement(ctx, scheduler.git, upstream.QualifiedRepo(repository.Name), candidateSHA)
	if err != nil {
		return err
	}
	if requirement.Contract != nil {
		return fmt.Errorf("%w: trusted test profile %s requires a protected suite executor and verified result evidence", ErrUnavailable, requirement.Contract.Profile)
	}
	return nil
}

func (scheduler *Scheduler) requirePublicationTestPolicy(ctx context.Context, repository model.Repository, publication model.Publication) error {
	candidateSHA := publication.ResultSHA
	if publication.RunID != "" {
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
	return scheduler.requireTrustedTestPolicy(ctx, repository, candidateSHA)
}
