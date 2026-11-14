package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
)

type ContainerLogSource interface {
	ContainerLogs(context.Context, model.Run) ([]runlog.ContainerLog, error)
	OpenContainerLog(context.Context, model.Run, string, string, bool) (io.ReadCloser, error)
}

type ContainerLogInfo struct {
	NodeID    string `json:"node_id"`
	Stage     string `json:"burn,omitempty"`
	Step      string `json:"step,omitempty"`
	PodName   string `json:"pod_name,omitempty"`
	Container string `json:"container"`
	Stored    bool   `json:"stored"`
	Reason    string `json:"reason,omitempty"`
}

type ContainerLogsResponse struct {
	RunID      string             `json:"run_id"`
	Containers []ContainerLogInfo `json:"containers"`
}

type ContainerLogResponse struct {
	LogResponse
	NodeID    string `json:"node_id"`
	Container string `json:"container"`
}

func (service *API) containerLogRun(ctx context.Context, actor api.Actor, id string) (*API, model.Run, error) {
	scoped, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, model.Run{}, err
	}
	if scoped.logs == nil || scoped.history == nil {
		return nil, model.Run{}, refuse(ErrUnavailable, "logs")
	}
	run, err := scoped.resolveReadRunID(ctx, id)
	if err != nil {
		return nil, run, err
	}
	if err := scoped.purgedEvidence(ctx, run, "logs"); err != nil {
		return nil, run, err
	}
	return scoped, run, nil
}

func (service *API) CheckContainerLogAccess(ctx context.Context, actor api.Actor, id string) error {
	_, _, err := service.containerLogRun(ctx, actor, id)
	return err
}

func (service *API) RunContainerLogs(ctx context.Context, actor api.Actor, id string) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, runReadBudget)
	defer cancel()
	scoped, run, err := service.containerLogRun(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	entries, err := scoped.logs.ContainerLogs(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	if run.WorkflowName != "" && run.Status.Active() && scoped.containerLogSource != nil {
		current, sourceErr := scoped.containerLogSource.ContainerLogs(ctx, run)
		if sourceErr != nil && len(entries) == 0 {
			return nil, fmt.Errorf("%w: native container observation is unavailable", ErrUnavailable)
		}
		for _, entry := range current {
			found := false
			for _, saved := range entries {
				if saved.PodUID == entry.PodUID && saved.ContainerName == entry.ContainerName {
					found = true
					break
				}
			}
			if !found {
				entries = append(entries, entry)
			}
		}
	}
	result := ContainerLogsResponse{RunID: run.ID, Containers: []ContainerLogInfo{}}
	for _, entry := range entries {
		if helperLogContainer(entry.ContainerName) {
			continue
		}
		result.Containers = append(result.Containers, ContainerLogInfo{NodeID: entry.NodeID, Stage: entry.Stage, Step: entry.Step, PodName: entry.PodName, Container: entry.ContainerName, Stored: entry.Captured, Reason: containerLogReason(run, entry)})
	}
	sort.Slice(result.Containers, func(i, j int) bool {
		a, b := result.Containers[i], result.Containers[j]
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		return a.Container < b.Container
	})
	return result, nil
}

func (service *API) OpenRunContainerLog(ctx context.Context, actor api.Actor, id, node, container string, follow bool) (io.ReadCloser, error) {
	authCtx, cancel := context.WithTimeout(ctx, runReadBudget)
	scoped, run, err := service.containerLogRun(authCtx, actor, id)
	cancel()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(node) == "" {
		return nil, fmt.Errorf("%w: node_id is required", ErrInvalidInput)
	}
	stored, storedErr := scoped.logs.OpenStoredContainerLog(ctx, run.ID, node, container)
	if storedErr == nil {
		return stored, nil
	}
	if !errors.Is(storedErr, os.ErrNotExist) {
		return nil, storedErr
	}
	if run.Status.Terminal() {
		return scoped.openUncapturedTerminalContainerLog(ctx, run, node, container)
	}
	if !follow {
		return nil, storedErr
	}
	if scoped.containerLogSource == nil {
		return nil, refuse(ErrUnavailable, "native container logs")
	}
	return scoped.containerLogSource.OpenContainerLog(ctx, run, node, container, true)
}

func containerLogReason(run model.Run, entry runlog.ContainerLog) string {
	if run.Status.Terminal() && !entry.Captured && entry.Reason == "" {
		return "oberth: not captured: observation ended"
	}
	return entry.Reason
}

func (service *API) openUncapturedTerminalContainerLog(ctx context.Context, run model.Run, node, container string) (io.ReadCloser, error) {
	entries, err := service.logs.ContainerLogs(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.NodeID == node && entry.ContainerName == container {
			if reason := containerLogReason(run, entry); reason != "" {
				return io.NopCloser(strings.NewReader(reason + "\n")), nil
			}
			break
		}
	}
	return nil, os.ErrNotExist
}

func (service *API) RunContainerLogFiltered(ctx context.Context, actor api.Actor, id, node, container string, filter runlog.Filter) (any, error) {
	if strings.TrimSpace(node) == "" {
		return nil, fmt.Errorf("%w: node_id is required", ErrInvalidInput)
	}
	ctx = withRunLogCaller(ctx, actor)
	ctx, cancel := context.WithTimeout(ctx, runReadBudget)
	defer cancel()
	scoped, run, err := service.containerLogRun(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if filter.Pattern != "" {
		release, err := runLogPatternScans.acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	body, meta, entry, err := scoped.logs.ReadContainerLog(ctx, run.ID, node, container, filter.WithContext(ctx))
	if errors.Is(err, os.ErrNotExist) && run.Status.Terminal() && entry.NodeID == node && entry.ContainerName == container {
		if reason := containerLogReason(run, entry); reason != "" {
			body, meta, err = runlog.FilterBytes([]byte(reason+"\n"), filter.WithContext(ctx))
		}
	}
	if errors.Is(err, os.ErrNotExist) && run.Status.Active() {
		return nil, fmt.Errorf("%w: stored container log is pending; request the live container stream", ErrStepLogPending)
	}
	if err != nil {
		return nil, boundedRunReadError(err)
	}
	response := ContainerLogResponse{LogResponse: newLogResponse(run.ID, "", "", body, meta), NodeID: node, Container: container}
	return response, nil
}

func (service *API) rawRunLog(ctx context.Context, actor api.Actor, id string, filter runlog.Filter) (any, error) {
	ctx = withRunLogCaller(ctx, actor)
	ctx, cancel := context.WithTimeout(ctx, runReadBudget)
	defer cancel()
	scoped, run, err := service.containerLogRun(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if filter.Pattern != "" {
		release, err := runLogPatternScans.acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	body, meta, err := scoped.logs.ReadRunLog(ctx, run.ID, filter.WithContext(ctx))
	if err != nil {
		return nil, boundedRunReadError(err)
	}
	return newLogResponse(run.ID, "", "", body, meta), nil
}

// Native helpers can log resolved templates, output values and error bodies.
// Keep them out of default discovery; an explicitly selected helper remains
// available through the same scoped, authenticated native reader.
func helperLogContainer(name string) bool {
	return name == "init" || name == "wait" || name == "oberth-executor-token" || strings.HasPrefix(name, "init-artifact-")
}
