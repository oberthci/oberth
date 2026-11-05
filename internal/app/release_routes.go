package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func (jobs *ArgoJobs) runSourceFile(request service.WorkflowRequest) (string, error) {
	trigger, err := runTrigger(request.Run)
	if err != nil {
		return "", err
	}
	if trigger != pipeline.TriggerRelease {
		return argoworkflow.TriggerFile(trigger)
	}
	// Preserve legacy behavior for unconfigured repositories, including older
	// trusted callers without durable tag kinds. Configured routing requires a tag.
	repo := declarationRepo(request)
	if len(jobs.config.ReleaseTagRoutes[repo]) == 0 {
		return argoworkflow.ReleaseFile, nil
	}
	return jobs.config.ReleaseTagRoutes.Resolve(repo, request.Run.RefKind, request.Run.Ref)
}

func (jobs *ArgoJobs) readRunSource(request service.WorkflowRequest) ([]byte, error) {
	if jobs.targetBinding != nil {
		if err := matchTargetRun(*jobs.targetBinding, request.Run, request.WorkflowName); err != nil {
			return nil, err
		}
		source, err := readBoundedSourceFile(request.SourceDir, argoworkflow.BuildFile)
		if err != nil {
			return nil, err
		}
		if targetBytesHash(source) != jobs.targetBinding.SourceDigest {
			return nil, errors.New("app: checkout differs from retained source")
		}
		return append([]byte(nil), jobs.targetSource...), nil
	}

	file, err := jobs.runSourceFile(request)
	if err != nil {
		return nil, err
	}
	return readBoundedSourceFile(request.SourceDir, file)
}

// ReleasePreflightFiles makes all admin-routed files participate in the same
// branch warning / promotion refusal checks as the default release document.
func (jobs *ArgoJobs) ReleasePreflightFiles(_ context.Context, request service.WorkflowRequest) ([]string, error) {
	return jobs.config.ReleaseTagRoutes.Files(declarationRepo(request)), nil
}

func (jobs *ArgoJobs) noRunPipelineError(err error, request service.WorkflowRequest) error {
	trigger, triggerErr := runTrigger(request.Run)
	if triggerErr != nil {
		return triggerErr
	}
	file, routeErr := jobs.runSourceFile(request)
	if routeErr != nil {
		return routeErr
	}
	if errors.Is(err, os.ErrNotExist) && trigger == pipeline.TriggerRelease && file != argoworkflow.ReleaseFile {
		return fmt.Errorf("repository %q has no administrator-routed release workflow %q: %w", request.Repository.Name, file, err)
	}
	return noPipelineError(err, trigger, request.Repository.Name)
}
