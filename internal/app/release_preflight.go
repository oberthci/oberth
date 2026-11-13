package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func (jobs *ArgoJobs) PreflightRelease(ctx context.Context, request service.WorkflowRequest, source []byte) (service.ReleasePreflightResult, error) {
	return jobs.preflightRelease(ctx, request, source, true)
}

// PlanRelease inspects public release capabilities without recording declarations.
func (jobs *ArgoJobs) PlanRelease(ctx context.Context, request service.WorkflowRequest, source []byte) (service.ReleasePreflightResult, error) {
	return jobs.preflightRelease(ctx, request, source, false)
}

func (jobs *ArgoJobs) preflightRelease(ctx context.Context, request service.WorkflowRequest, source []byte, record bool) (service.ReleasePreflightResult, error) {
	if request.PipelineFile == "" {
		request.PipelineFile = argoworkflow.ReleaseFile
	}
	jobs.mu.Lock()
	identities, recorder := jobs.identities, jobs.declarations
	jobs.mu.Unlock()
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return service.ReleasePreflightResult{}, err
	}
	grants := map[string]map[string]bool{}
	approved := map[string]bool{}
	if jobs.secretAccess != nil {
		var accessErr error
		approved, grants, accessErr = jobs.stepSecretAccess(ctx, request.Repository, workflow, request.PipelineFile)
		var denied *argoworkflow.AdmissionError
		if accessErr != nil && !errors.As(accessErr, &denied) {
			return service.ReleasePreflightResult{}, accessErr
		}
	}
	submission := argojob.Request{Repo: request.Repository.Name, UpstreamName: request.UpstreamName, UpstreamOrg: request.UpstreamOrg,
		Source: source, PipelineFile: request.PipelineFile, Trigger: pipeline.TriggerRelease, ApprovedSecrets: approved, StepGrants: grants}
	submission.RepositoryID = request.Repository.ID
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return service.ReleasePreflightResult{}, err
	}
	if argoworkflow.CapabilityUsage(workflow) && jobs.capabilityGrants != nil {
		capability, err := jobs.capabilityGrants(ctx, request.Repository.ID)
		if err != nil {
			return service.ReleasePreflightResult{}, err
		}
		if capability != nil && capability.Namespace != jobs.config.Namespace {
			return service.ReleasePreflightResult{}, fmt.Errorf("app: workflow capability belongs to a different execution namespace")
		}
		submission.Capability = capability
	}
	if identities != nil {
		submission.Identities = identities.Snapshot()
	}
	submission.ReleaseWIF = jobs.releaseWIFSnapshot()
	result, err := argojob.PreflightRelease(jobs.config, submission)
	if err != nil {
		return service.ReleasePreflightResult{}, err
	}
	if record && recorder != nil {
		declarations := make([]store.GrantDeclaration, 0, len(result.Declarations))
		for _, d := range result.Declarations {
			declarations = append(declarations, store.GrantDeclaration{Workflow: request.PipelineFile, Step: d.Step, Path: d.Path})
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
