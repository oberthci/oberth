package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func (jobs *ArgoJobs) runSourceFile(request service.JobRequest) (string, error) {
	trigger, err := runTrigger(request.Run)
	if err != nil {
		return "", err
	}
	if trigger != periapsis.TriggerRelease {
		return argoworkflow.TriggerFile(trigger)
	}
	// Preserve legacy behavior for unconfigured repositories, including older
	// trusted callers without full tag refs. Configured routing requires a tag.
	repo := declarationRepo(request)
	if len(jobs.config.ReleaseTagRoutes[repo]) == 0 {
		return argoworkflow.ReleaseFile, nil
	}
	return jobs.config.ReleaseTagRoutes.Resolve(repo, request.Run.Ref)
}

func (jobs *ArgoJobs) readRunSource(request service.JobRequest) ([]byte, error) {
	file, err := jobs.runSourceFile(request)
	if err != nil {
		return nil, err
	}
	return readBoundedSourceFile(request.SourceDir, file)
}

// ReleasePreflightFiles makes all admin-routed files participate in the same
// branch warning / promotion refusal checks as the default release document.
func (jobs *ArgoJobs) ReleasePreflightFiles(_ context.Context, request service.JobRequest) ([]string, error) {
	return jobs.config.ReleaseTagRoutes.Files(declarationRepo(request)), nil
}

func (jobs *ArgoJobs) noRunPipelineError(err error, request service.JobRequest) error {
	trigger, triggerErr := runTrigger(request.Run)
	if triggerErr != nil {
		return triggerErr
	}
	file, routeErr := jobs.runSourceFile(request)
	if routeErr != nil {
		return routeErr
	}
	if errors.Is(err, os.ErrNotExist) && trigger == periapsis.TriggerRelease && file != argoworkflow.ReleaseFile {
		return fmt.Errorf("repository %q has no administrator-routed release workflow %q: %w", request.Repository.Name, file, err)
	}
	return noPipelineError(err, trigger, request.Repository.Name)
}
