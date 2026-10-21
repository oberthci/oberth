package app

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func (jobs *ArgoJobs) PreflightRelease(ctx context.Context, request service.JobRequest, source []byte) (service.ReleasePreflightResult, error) {
	return jobs.preflightRelease(ctx, request, source, true)
}

// PlanRelease inspects public release capabilities without recording declarations.
func (jobs *ArgoJobs) PlanRelease(ctx context.Context, request service.JobRequest, source []byte) (service.ReleasePreflightResult, error) {
	return jobs.preflightRelease(ctx, request, source, false)
}

func (jobs *ArgoJobs) preflightRelease(ctx context.Context, request service.JobRequest, source []byte, record bool) (service.ReleasePreflightResult, error) {
	jobs.mu.Lock()
	health, identities, recorder := jobs.reconcilerHealthy, jobs.identities, jobs.declarations
	jobs.mu.Unlock()
	if health != nil && !health.ReconcileHealthy() {
		return service.ReleasePreflightResult{}, fmt.Errorf("secret access reconciliation is not healthy")
	}
	grants := map[string]map[string]bool{}
	approved := map[string]bool{}
	if jobs.secretAccess != nil {
		loaded, err := jobs.secretAccess.ActiveSecretGrants(ctx, request.Repository.ID)
		if err != nil {
			return service.ReleasePreflightResult{}, err
		}
		for step, paths := range loaded {
			for path := range paths {
				approved[path] = true
				if grants[path] == nil {
					grants[path] = map[string]bool{}
				}
				grants[path][step] = true
			}
		}
	}
	fragments, err := loadFragments(ctx, jobs.fragments, source, periapsis.TriggerRelease, request.Run.RefKind, "release")
	if err != nil {
		return service.ReleasePreflightResult{}, err
	}
	submission := argojob.Request{Repo: request.Repository.Name, UpstreamName: request.UpstreamName, UpstreamOrg: request.UpstreamOrg,
		Source: source, Trigger: periapsis.TriggerRelease, ApprovedSecrets: approved, StepGrants: grants, Fragments: fragments}
	if identities != nil {
		submission.Identities = identities.Snapshot()
	}
	result, err := argojob.PreflightRelease(jobs.config, submission)
	if err != nil {
		return service.ReleasePreflightResult{}, err
	}
	if record && recorder != nil {
		declarations := make([]store.GrantDeclaration, 0, len(result.Declarations))
		for _, d := range result.Declarations {
			declarations = append(declarations, store.GrantDeclaration{Step: d.Step, Path: d.Path})
		}
		repo := request.UpstreamName + "/" + request.UpstreamOrg + "/" + request.Repository.Name
		sha := request.Run.TestedSHA
		if sha == "" {
			sha = request.Run.SHA
		}
		if err := recorder.RecordGrantDeclarations(ctx, repo, sha, declarations); err != nil {
			return service.ReleasePreflightResult{}, fmt.Errorf("record release declarations: %w", err)
		}
	}
	if checker, ok := jobs.controller.(interface {
		PreflightReleaseAccounts(context.Context, argojob.Request) error
	}); ok {
		if err := checker.PreflightReleaseAccounts(ctx, submission); err != nil {
			result.Problems = append(result.Problems, err.Error())
		}
	}
	return service.ReleasePreflightResult{Problems: result.Problems, Checks: result.Checks}, nil
}
