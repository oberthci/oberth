package app

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// WatchFailedCreate observes only the immutable run identity, without rebuilding
// the authored Workflow or reissuing a create.
func (jobs *ArgoJobs) WatchFailedCreate(ctx context.Context, request service.WorkflowRequest) error {
	watcher, ok := jobs.controller.(interface {
		WatchFailedCreate(context.Context, argojob.Request) error
	})
	if !ok {
		return errors.New("app: Workflow controller cannot observe a failed create")
	}
	sha := request.Run.TestedSHA
	if sha == "" {
		sha = request.Run.SHA
	}
	trigger := pipeline.TriggerCI
	if request.Run.Release {
		trigger = pipeline.TriggerRelease
	}
	return watcher.WatchFailedCreate(ctx, argojob.Request{
		RunID: request.Run.ID, RepositoryID: request.Repository.ID, Name: request.WorkflowName,
		Repo: request.Repository.Name, Ref: request.Run.Ref, SHA: sha, Trigger: trigger,
		RunTrigger: argoworkflow.RunTrigger(request.Run.Trigger),
	})
}

// ActiveWorkflowRuns uses the synced namespace hub without a Kubernetes list.
func (jobs *ArgoJobs) ActiveWorkflowRuns() []service.WorkflowRunRef {
	if jobs.watchHub == nil {
		return nil
	}
	var runs []service.WorkflowRunRef
	for _, wf := range jobs.watchHub.ActiveWorkflows() {
		runID, _, ok := argojob.WorkflowMeta(wf)
		if ok {
			runs = append(runs, service.WorkflowRunRef{RunID: runID, WorkflowName: wf.Name})
		}
	}
	return runs
}
