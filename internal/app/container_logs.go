package app

import (
	"context"
	"io"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/service"
)

type containerLogController interface {
	ContainerLogTargets(context.Context, string, string) ([]argojob.StepContainer, error)
	OpenContainerLog(context.Context, string, string, string, string, bool) (io.ReadCloser, error)
}

func (jobs *ArgoJobs) SetContainerLogSource(source containerLogController) {
	jobs.containerLogSource = source
}

func (jobs *ArgoJobs) ContainerLogs(ctx context.Context, run model.Run) ([]runlog.ContainerLog, error) {
	if jobs.containerLogSource == nil {
		return nil, service.ErrUnavailable
	}
	targets, err := jobs.containerLogSource.ContainerLogTargets(ctx, run.WorkflowName, run.ID)
	if err != nil {
		return nil, err
	}
	result := make([]runlog.ContainerLog, 0, len(targets))
	for _, target := range targets {
		result = append(result, runlog.ContainerLog{NodeID: target.NodeID, Namespace: target.Namespace, PodName: target.PodName, PodUID: string(target.PodUID), ContainerName: target.ContainerName})
	}
	return result, nil
}

func (jobs *ArgoJobs) OpenContainerLog(ctx context.Context, run model.Run, node, container string, follow bool) (io.ReadCloser, error) {
	if jobs.containerLogSource == nil {
		return nil, service.ErrUnavailable
	}
	return jobs.containerLogSource.OpenContainerLog(ctx, run.WorkflowName, run.ID, node, container, follow)
}
