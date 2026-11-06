package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
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
	return p.verifyPlacedFragmentAuthority(gitcache.WithFreshPolicy(ctx), a)
}

func (p *TrustedPreflights) checkPlacedPreparation(job service.WorkflowRequest, requirement vmrunner.Requirement, recipe vmrunner.KVMRecipe, capability *argoworkflow.Capability, source []byte, fragments map[argoworkflow.FragmentKey]argoworkflow.Fragment) error {
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
	r := argojob.Request{RunID: job.Run.ID, RepositoryID: job.Repository.ID, Name: job.WorkflowName, Repo: job.Repository.Name, UpstreamName: job.UpstreamName, UpstreamOrg: job.UpstreamOrg, Ref: job.Run.Ref, SHA: requirement.CandidateSHA, PipelineFile: ".oberth/build.yaml", Trigger: pipeline.TriggerCI, RunTrigger: kind, ScheduleEntry: job.Run.ScheduleEntry, Source: source, Fragments: fragments, Capability: capability}
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

// Recovery uses the atomically admitted original graph, never current tag
// resolution. Returning bytes cannot grant create or receipt authority.
func (p *TrustedPreflights) recoverySources(ctx context.Context, job service.WorkflowRequest, runtime *protectedtests.GenericAdmission) ([]byte, map[argoworkflow.FragmentKey]argoworkflow.Fragment, error) {
	if runtime != nil {
		if journal, ok := p.store.(argojob.ProtectedPlacementJournal); ok {
			admission, err := journal.PlacementAdmission(ctx, runtime.Intent.RunID)
			if err != nil {
				return nil, nil, err
			}
			if admission != nil && admission.Sources != nil {
				return admission.RestoreSources(*runtime)
			}
		}
	}
	source, err := readArgoSource(job.SourceDir, pipeline.TriggerCI)
	return source, nil, err
}

func (p *TrustedPreflights) verifyPlacedFragmentAuthority(ctx context.Context, runtime protectedtests.GenericAdmission) error {
	journal, ok := p.store.(argojob.ProtectedPlacementJournal)
	if !ok {
		return errors.New("protected fragment custody journal unavailable")
	}
	retained, err := journal.PlacementAdmission(ctx, runtime.Intent.RunID)
	if err != nil {
		return err
	}
	if retained == nil || retained.Sources == nil {
		return nil
	}
	source, present, err := p.jobs.resourceBlobs.ReadOptionalBlob(ctx, runtime.Intent.Repository, runtime.Intent.CandidateSHA, argoworkflow.BuildFile, argoworkflow.MaxSourceBytes)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("current protected fragment root is missing")
	}
	// Refresh each original producer's authenticated private default and tag
	// refs. Protected fragment authority then resolves only those private tags:
	// a removed or moved upstream tag must not retain public cached authority.
	// Failed refreshes refuse; ordinary fragment loading keeps its public refs.
	refreshed := map[string]bool{}
	for _, fragment := range retained.Sources.Fragments {
		key, err := argoworkflow.ParseFragmentRef(fragment.Key)
		if err != nil {
			return err
		}
		if !refreshed[key.Repo] {
			if _, _, _, err = p.jobs.resourceBlobs.ReadDefaultBlob(ctx, key.Repo, argoworkflow.FragmentFile, argoworkflow.MaxSourceBytes); err != nil {
				return err
			}
			refreshed[key.Repo] = true
		}
	}
	fragments, err := loadFragments(gitcache.WithUpstreamFragmentTags(ctx), p.jobs.fragments, source, pipeline.TriggerCI, model.RefBranch, "branch")
	if err != nil {
		return err
	}
	current, err := protectedtests.NewFragmentPlacementAdmission(retained.Placement, retained.Config, runtime, source, fragments)
	if err != nil {
		return err
	}
	if current.Digest() != retained.Digest() {
		return errors.New("current protected fragment authority differs from retained graph")
	}
	return nil
}
