package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func (p *TrustedPreflights) verifyPlacedAuthority(ctx context.Context, a protectedtests.GenericAdmission) error {
	if !p.generic || p.jobs.config.TrustedKVMGroup <= 0 || p.jobs.resourceBlobs == nil {
		return errors.New("protected placement current generic policy unavailable")
	}
	if err := p.requireMutation(ctx); err != nil {
		return err
	}
	if err := p.RecipeAllowed(a.Intent.Recipe); err != nil {
		return err
	}
	required, err := vmrunner.ResolveRequirement(gitcache.WithFreshPolicy(ctx), p.jobs.resourceBlobs, a.Intent.Repository, a.Intent.CandidateSHA, p.Supports)
	if err != nil {
		return err
	}
	descriptor, err := a.Intent.Descriptor()
	if err != nil {
		return err
	}
	if required.BaselineSHA != a.Intent.BaselineSHA || descriptor.MatchContract(required.Contract) != nil {
		return errors.New("protected placement canonical baseline or contract changed")
	}
	return nil
}

func (p *TrustedPreflights) checkPlacedPreparation(job service.WorkflowRequest, requirement vmrunner.Requirement, recipe vmrunner.KVMRecipe, capability *argoworkflow.Capability, source []byte) error {
	placement := p.jobs.config.ExecutionPlacement
	if !placement.Protected() {
		return nil
	}
	if !p.generic || job.Run.Release || job.Run.Credentialed {
		return errors.New("protected placement requires ordinary generic branch execution")
	}
	kind, err := runKind(job.Run)
	if err != nil {
		return err
	}
	r := argojob.Request{RunID: job.Run.ID, RepositoryID: job.Repository.ID, Name: job.WorkflowName, Repo: job.Repository.Name, UpstreamName: job.UpstreamName, UpstreamOrg: job.UpstreamOrg, Ref: job.Run.Ref, SHA: requirement.CandidateSHA, PipelineFile: ".oberth/build.yaml", Trigger: pipeline.TriggerCI, RunTrigger: kind, ScheduleEntry: job.Run.ScheduleEntry, Source: source, Capability: capability}
	if err = placement.ValidateRequest(p.jobs.config, r); err != nil {
		return err
	}
	return placement.ValidateProtectedPolicy(requirement.BaselineSHA, requirement.Contract, recipe)
}

// CheckProtectedPlacementAuthority fences scheduler delivery/publication against
// current primary configuration as well as the stored runtime grant and expiry.
func (p *TrustedPreflights) CheckProtectedPlacementAuthority(ctx context.Context, runID string) error {
	journal, ok := p.store.(argojob.ProtectedPlacementJournal)
	if !ok {
		if p.jobs.config.ExecutionPlacement.Protected() {
			return errors.New("protected placement journal unavailable")
		}
		return nil
	}
	a, err := journal.PlacementAdmission(ctx, runID)
	if err != nil {
		return err
	}
	if a == nil {
		return nil
	}
	runtime, err := journal.GenericAdmission(ctx, runID)
	if err != nil {
		return err
	}
	if runtime == nil || a.Validate(*runtime) != nil {
		return errors.New("protected placement runtime missing")
	}
	config, err := argojob.DefaultedConfig(p.jobs.config)
	if err != nil {
		return err
	}
	body, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if !config.ExecutionPlacement.Protected() || string(body) != string(a.Config) {
		return errors.New("current primary placement differs from retained admission")
	}
	if err = config.ExecutionPlacement.ValidateAdmission(); err != nil {
		return err
	}
	if err = journal.CheckGenericPreflightAuthority(ctx, runID); err != nil {
		return err
	}
	controller, ok := p.controller.(interface {
		CheckProtectedPlacementAuthority(context.Context, string) error
	})
	if !ok {
		return errors.New("protected placement live environment verifier unavailable")
	}
	return controller.CheckProtectedPlacementAuthority(ctx, runID)
}
