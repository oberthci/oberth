package argojob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/runlog"
)

type PodLogStream func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error)

func (controller *Controller) WithContainerLogStore(store runlog.ContainerLogStore) *Controller {
	controller.containerLogs = store
	return controller
}

func (controller *Controller) WithContainerLogSource(open PodLogStream, pod func(context.Context, string, string) (*corev1.Pod, error)) *Controller {
	controller.containerLogSource, controller.containerLogPod = open, pod
	return controller
}

func containerLogEntry(target StepContainer) runlog.ContainerLog {
	return runlog.ContainerLog{NodeID: target.NodeID, Namespace: target.Namespace, PodUID: string(target.PodUID), PodName: target.PodName, ContainerName: target.ContainerName}
}

func containerFinished(pod *corev1.Pod, name string) bool {
	if pod == nil {
		return false
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, status := range statuses {
			if status.Name == name && status.State.Terminated != nil {
				return true
			}
		}
	}
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func containerLogDone(workflow *wfv1.Workflow, entry runlog.ContainerLog, pods []*corev1.Pod) bool {
	for _, pod := range pods {
		if pod != nil && string(pod.UID) == entry.PodUID {
			return containerFinished(pod, entry.ContainerName)
		}
	}
	node, exists := workflow.Status.Nodes[entry.NodeID]
	return workflow.Status.Phase.Completed() || (exists && node.Fulfilled())
}

// ObserveContainerLogs runs in the existing verified Workflow observation. It
// opens no follower, timer or subscriber. Each terminal container is read once.
func (controller *Controller) ObserveContainerLogs(ctx context.Context, workflow *wfv1.Workflow, runID string, pods []*corev1.Pod) error {
	if controller.containerLogs == nil {
		return nil
	}
	if workflow == nil {
		return errors.New("argojob: Workflow log identity is unavailable")
	}
	if err := controller.verifyOwnership(workflow, runID); err != nil {
		return err
	}
	if workflow.Namespace != controller.config.Namespace || workflow.UID == "" {
		return errors.New("argojob: invalid Workflow log identity")
	}
	var known []runlog.ContainerLog
	for _, target := range StepContainers(workflow, pods) {
		known = append(known, containerLogEntry(target))
	}
	if err := controller.containerLogs.TrackContainerLogs(runID, known); err != nil {
		return err
	}
	entries, err := controller.containerLogs.ContainerLogs(ctx, runID)
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if ctx.Err() != nil {
			return errors.Join(result, ctx.Err())
		}
		if entry.Captured || entry.Pending || !containerLogDone(workflow, entry, pods) {
			continue
		}
		claimed, err := controller.containerLogs.ReserveContainerLog(runID, entry)
		if err != nil {
			return errors.Join(result, err)
		}
		if !claimed {
			continue
		}
		entry.Body, entry.Reason = controller.readContainerCopy(ctx, workflow, entry)
		result = errors.Join(result, controller.containerLogs.StoreContainerLog(runID, entry, controller.config.MaxRunLogBytes))
	}
	return errors.Join(result, controller.recordMissingPodLogs(runID, workflow, entries))
}

func (controller *Controller) recordMissingPodLogs(runID string, workflow *wfv1.Workflow, entries []runlog.ContainerLog) error {
	if !workflow.Status.Phase.Completed() {
		return nil
	}
	observed := map[string]bool{}
	for _, entry := range entries {
		observed[entry.NodeID] = true
		if node, found := workflow.Status.Nodes[entry.NodeID]; found && node.BoundaryID != "" {
			observed[node.BoundaryID] = true
		}
	}
	var result error
	for id, node := range workflow.Status.Nodes {
		if node.Type != wfv1.NodeTypePod || !node.Fulfilled() || observed[id] {
			continue
		}
		entry := runlog.ContainerLog{NodeID: id, Reason: "oberth: log unavailable: Pod containers were not observed before Workflow completion"}
		result = errors.Join(result, controller.containerLogs.StoreContainerLog(runID, entry, controller.config.MaxRunLogBytes))
	}
	return result
}

func (controller *Controller) readContainerCopy(ctx context.Context, workflow *wfv1.Workflow, entry runlog.ContainerLog) ([]byte, string) {
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		reader, err := controller.openBoundContainerLog(readCtx, workflow, entry, false)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil, "oberth: log unavailable: Pod or container log no longer exists"
			}
			if attempt == 0 && retryExecutorIdentityRead(err) && readCtx.Err() == nil {
				continue
			}
			return nil, "oberth: log unavailable: container log could not be read"
		}
		body, readErr := io.ReadAll(io.LimitReader(reader, controller.config.MaxRunLogBytes+1))
		_ = reader.Close()
		if readErr != nil {
			if len(body) == 0 && attempt == 0 && retryExecutorIdentityRead(readErr) && readCtx.Err() == nil {
				continue
			}
			return body, "oberth: log incomplete: container log read was interrupted"
		}
		return body, ""
	}
	return nil, "oberth: log unavailable: container log could not be read"
}

func (controller *Controller) verifyContainerLogPod(ctx context.Context, workflow *wfv1.Workflow, entry runlog.ContainerLog) error {
	if controller.containerLogPod == nil {
		return errors.New("argojob: native Pod log reader is unavailable")
	}
	pod, err := controller.containerLogPod(ctx, entry.Namespace, entry.PodName)
	if err != nil {
		return err
	}
	for _, target := range StepContainers(workflow, []*corev1.Pod{pod}) {
		if target.NodeID == entry.NodeID && string(target.PodUID) == entry.PodUID && target.ContainerName == entry.ContainerName && target.Namespace == entry.Namespace && target.PodName == entry.PodName {
			return nil
		}
	}
	return errors.New("argojob: container log identity changed")
}

func (controller *Controller) openBoundContainerLog(ctx context.Context, workflow *wfv1.Workflow, entry runlog.ContainerLog, follow bool) (io.ReadCloser, error) {
	if controller.containerLogSource == nil {
		return nil, errors.New("argojob: native container logs are unavailable")
	}
	if err := controller.verifyContainerLogPod(ctx, workflow, entry); err != nil {
		return nil, err
	}
	reader, err := controller.containerLogSource(ctx, entry.Namespace, entry.PodName, corev1.PodLogOptions{Container: entry.ContainerName, Follow: follow})
	if err != nil {
		return nil, err
	}
	if err := controller.verifyContainerLogPod(ctx, workflow, entry); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return closeContainerLogWithContext(ctx, reader), nil
}

func (controller *Controller) ContainerLogTargets(ctx context.Context, name, runID string) ([]StepContainer, error) {
	workflow, err := controller.logWorkflow(ctx, name, runID)
	if err != nil {
		return nil, err
	}
	return StepContainers(workflow, controller.hub.PodsOf(name)), nil
}

func (controller *Controller) logWorkflow(ctx context.Context, name, runID string) (*wfv1.Workflow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if controller.hub == nil || !controller.hub.Healthy() {
		return nil, errWatchUnavailable
	}
	workflow, found := controller.hub.Workflow(name)
	if !found {
		var err error
		workflow, err = controller.workflows.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
	}
	if err := controller.verifyOwnership(workflow, runID); err != nil {
		return nil, err
	}
	if workflow.Namespace != controller.config.Namespace || workflow.UID == "" {
		return nil, errors.New("argojob: invalid Workflow log identity")
	}
	return workflow, nil
}

func (controller *Controller) OpenContainerLog(ctx context.Context, name, runID, nodeID, container string, follow bool) (io.ReadCloser, error) {
	workflow, err := controller.logWorkflow(ctx, name, runID)
	if err != nil {
		return nil, err
	}
	for _, target := range StepContainers(workflow, controller.hub.PodsOf(name)) {
		if target.NodeID == nodeID && target.ContainerName == container {
			return controller.openBoundContainerLog(ctx, workflow, containerLogEntry(target), follow)
		}
	}
	return nil, fmt.Errorf("argojob: native container log binding is unavailable: %w", apierrors.NewNotFound(corev1.Resource("pods"), nodeID))
}

type containerLogStream struct {
	io.Reader
	close func() error
}

func (stream *containerLogStream) Close() error { return stream.close() }

func closeContainerLogWithContext(ctx context.Context, reader io.ReadCloser) io.ReadCloser {
	var closeErr error
	closeOnce := sync.OnceFunc(func() { closeErr = reader.Close() })
	stop := context.AfterFunc(ctx, closeOnce)
	return &containerLogStream{Reader: reader, close: func() error { stop(); closeOnce(); return closeErr }}
}

func (controller *Controller) captureObservedContainerLogs(ctx context.Context, workflow *wfv1.Workflow, runID string) {
	if controller.containerLogs == nil || controller.hub == nil || !controller.hub.Healthy() {
		return
	}
	if err := controller.ObserveContainerLogs(ctx, workflow, runID, controller.hub.PodsOf(workflow.Name)); err != nil {
		log.Printf("WARN: run=%s container log copy unavailable: %v", runID, err)
	}
}
