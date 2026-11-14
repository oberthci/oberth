package argojob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
)

var errContainerLogObservationIncomplete = errors.New("argojob: container copy observation deadline ended")

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
	if workflow.Status.Phase.Completed() {
		return true
	}
	for _, pod := range pods {
		if pod != nil && string(pod.UID) == entry.PodUID {
			return containerFinished(pod, entry.ContainerName)
		}
	}
	node, exists := workflow.Status.Nodes[entry.NodeID]
	return workflow.Status.Phase.Completed() || (exists && node.Fulfilled())
}

// ObserveContainerLogs captures a verified snapshot outside status observation.
// Each terminal container is read once; completion also captures running Pods.
func (controller *Controller) ObserveContainerLogs(ctx context.Context, workflow *wfv1.Workflow, runID string, pods []*corev1.Pod) error {
	return controller.observeContainerLogs(ctx, workflow, runID, pods, 30*time.Second)
}

func (controller *Controller) observeContainerLogs(ctx context.Context, workflow *wfv1.Workflow, runID string, pods []*corev1.Pod, budget time.Duration) error {
	if controller.containerLogs == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
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
	expanded, err := nativeNodeWorkflow(ctx, workflow)
	if err != nil {
		return err
	}
	workflow = expanded
	names := map[string]model.StepResult{}
	for _, step := range NodeStepResults(workflow, pods) {
		names[step.NodeID] = step
	}
	var known []runlog.ContainerLog
	for _, target := range StepContainers(workflow, pods) {
		entry := containerLogEntry(target)
		step := names[target.NodeID]
		if step.Step == "" {
			step = names[workflow.Status.Nodes[target.NodeID].BoundaryID]
		}
		entry.Stage, entry.Step = step.Stage, step.Step
		if node := workflow.Status.Nodes[target.NodeID]; node.Type == wfv1.NodeTypeContainer {
			entry.ParentNodeID = node.BoundaryID
		}
		entry.Main = target.ContainerName == "main" || workflow.Status.Nodes[target.NodeID].Type == wfv1.NodeTypeContainer
		known = append(known, entry)
	}
	if err := controller.containerLogs.TrackContainerLogs(runID, known); err != nil {
		return err
	}
	entries, err := controller.containerLogs.ContainerLogs(ctx, runID)
	if err != nil {
		return err
	}
	readCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var result error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		if entry.Captured || entry.Pending || !containerLogDone(workflow, entry, pods) {
			continue
		}
		if readCtx.Err() != nil && !workflow.Status.Phase.Completed() {
			return errors.Join(result, errContainerLogObservationIncomplete)
		}
		claimed, err := controller.containerLogs.ReserveContainerLog(runID, entry)
		if err != nil {
			return errors.Join(result, err)
		}
		if !claimed {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err, controller.containerLogs.ReleaseContainerLog(runID, entry))
		}
		if readCtx.Err() != nil {
			entry.Reason = "oberth: log unavailable: container copy observation deadline ended"
			result = errors.Join(result, controller.containerLogs.StoreContainerLog(runID, entry, controller.config.MaxRunLogBytes))
			continue
		}
		if workflow.Status.Phase.Completed() {
			for _, pod := range pods {
				if pod != nil && string(pod.UID) == entry.PodUID && !containerFinished(pod, entry.ContainerName) {
					entry.Reason = "oberth: log incomplete: Workflow completed before container termination"
				}
			}
		}
		result = errors.Join(result, controller.storeContainerCopy(readCtx, ctx, workflow, runID, entry))
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(result, err)
	}
	return errors.Join(result, controller.recordMissingPodLogs(runID, workflow, entries, names))
}

func (controller *Controller) recordMissingPodLogs(runID string, workflow *wfv1.Workflow, entries []runlog.ContainerLog, names map[string]model.StepResult) error {
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
		if node.Type != wfv1.NodeTypePod || observed[id] {
			continue
		}
		entry := runlog.ContainerLog{NodeID: id, Stage: names[id].Stage, Step: names[id].Step, Reason: "oberth: log unavailable: Pod containers were not observed before Workflow completion"}
		result = errors.Join(result, controller.containerLogs.StoreContainerLog(runID, entry, controller.config.MaxRunLogBytes))
	}
	return result
}

func (controller *Controller) storeContainerCopy(ctx, observerCtx context.Context, workflow *wfv1.Workflow, runID string, entry runlog.ContainerLog) error {
	for attempt := 0; attempt < 2; attempt++ {
		reader, opened, err := controller.openBoundContainerLog(ctx, workflow, entry, false)
		if err == nil {
			entry.Reader = reader
			defer func() { _ = reader.Close() }()
			return controller.containerLogs.StoreContainerLog(runID, entry, controller.config.MaxRunLogBytes)
		}
		if !opened && observerCtx.Err() != nil {
			return errors.Join(observerCtx.Err(), controller.containerLogs.ReleaseContainerLog(runID, entry))
		}
		if apierrors.IsNotFound(err) {
			entry.Reason = "oberth: log unavailable: Pod or container log no longer exists"
			break
		}
		if attempt == 0 && retryExecutorIdentityRead(err) && ctx.Err() == nil {
			continue
		}
		entry.Reason = "oberth: log unavailable: container log could not be read"
		break
	}
	return controller.containerLogs.StoreContainerLog(runID, entry, controller.config.MaxRunLogBytes)
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

func (controller *Controller) openBoundContainerLog(ctx context.Context, workflow *wfv1.Workflow, entry runlog.ContainerLog, follow bool) (io.ReadCloser, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if controller.containerLogSource == nil {
		return nil, false, errors.New("argojob: native container logs are unavailable")
	}
	if err := controller.verifyContainerLogPod(ctx, workflow, entry); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	reader, err := controller.containerLogSource(ctx, entry.Namespace, entry.PodName, corev1.PodLogOptions{Container: entry.ContainerName, Follow: follow})
	if err != nil {
		return nil, false, err
	}
	if err := controller.verifyContainerLogPod(ctx, workflow, entry); err != nil {
		_ = reader.Close()
		return nil, true, err
	}
	return closeContainerLogWithContext(ctx, reader), true, nil
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
	return nativeNodeWorkflow(ctx, workflow)
}

func (controller *Controller) OpenContainerLog(ctx context.Context, name, runID, nodeID, container string, follow bool) (io.ReadCloser, error) {
	workflow, err := controller.logWorkflow(ctx, name, runID)
	if err != nil {
		return nil, err
	}
	for _, target := range StepContainers(workflow, controller.hub.PodsOf(name)) {
		if target.NodeID == nodeID && target.ContainerName == container {
			reader, _, err := controller.openBoundContainerLog(ctx, workflow, containerLogEntry(target), follow)
			return reader, err
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

type containerLogObservation struct {
	workflow *wfv1.Workflow
	pods     []*corev1.Pod
}

// containerLogCapture owns one serial copy task for Wait. Its one-slot mailbox
// coalesces snapshots; no network reads or append locks run on the observation
// goroutine. Joining happens only after terminal identity retirement.
func (controller *Controller) containerLogCapture(ctx context.Context, runID string) (func(*wfv1.Workflow), func()) {
	if controller.containerLogs == nil {
		return func(*wfv1.Workflow) {}, func() {}
	}
	snapshots := make(chan containerLogObservation, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		previous := ""
		for snapshot := range snapshots {
			version := containerLogObservationVersion(snapshot)
			if version != "" && version == previous && !snapshot.workflow.Status.Phase.Completed() {
				continue
			}
			if err := controller.ObserveContainerLogs(ctx, snapshot.workflow, runID, snapshot.pods); err != nil {
				if ctx.Err() == nil && !errors.Is(err, errContainerLogObservationIncomplete) {
					log.Printf("WARN: run=%s container log copy unavailable (class=%s)", strconv.Quote(runID), executorIdentityErrorClass(err))
				}
			} else {
				previous = version
			}
		}
	}()
	observe := func(workflow *wfv1.Workflow) {
		snapshot := containerLogObservation{workflow: workflow.DeepCopy()}
		if controller.hub != nil && controller.hub.Healthy() {
			snapshot.pods = controller.hub.PodsOf(workflow.Name)
		}
		// A single producer owns this channel. Keep the most recent observation,
		// especially the completed snapshot, while a previous copy is blocked.
		select {
		case snapshots <- snapshot:
			return
		default:
		}
		select {
		case <-snapshots:
		default:
		}
		snapshots <- snapshot
	}
	return observe, func() {
		close(snapshots)
		<-joined
		// Cancellation transfers unfinished observation to the next owner.
		// Already attempted copies keep the receipts written by the joined task.
		if ctx.Err() == nil {
			controller.finishContainerLogs(runID)
		}
	}
}

// The copy task has joined, so tracked entries without a copy can be explained
// even when identity or delivery errors ended Wait before Workflow completion.
func (controller *Controller) finishContainerLogs(runID string) {
	entries, err := controller.containerLogs.ContainerLogs(context.Background(), runID)
	if err == nil {
		for _, entry := range entries {
			if entry.Captured {
				continue
			}
			entry.Reason = "oberth: not captured: observation ended"
			err = errors.Join(err, controller.containerLogs.StoreContainerLog(runID, entry, controller.config.MaxRunLogBytes))
		}
	}
	if err != nil {
		log.Printf("WARN: run=%s container log finalization unavailable (class=%s)", strconv.Quote(runID), executorIdentityErrorClass(err))
	}
}

// Real watch objects always have resource versions. Fixtures without them are
// deliberately not cached. A changed Pod state must trigger a new observation.
func containerLogObservationVersion(snapshot containerLogObservation) string {
	if snapshot.workflow.ResourceVersion == "" {
		return ""
	}
	versions := []string{snapshot.workflow.ResourceVersion}
	for _, pod := range snapshot.pods {
		if pod == nil || pod.ResourceVersion == "" {
			return ""
		}
		versions = append(versions, string(pod.UID)+"/"+pod.ResourceVersion)
	}
	sort.Strings(versions[1:])
	return strings.Join(versions, "\x00")
}
