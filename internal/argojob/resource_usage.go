package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/resourcesampler"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type ResourceUsageRecorder interface {
	RecordStepResourceUsage(context.Context, []model.StepResourceUsage) error
}
type ResourceUsageSource interface {
	Interval() time.Duration
	Snapshot(context.Context, string) (resourcesampler.Snapshot, error)
}

func (c *Controller) WithResourceSampler(source ResourceUsageSource, recorder ResourceUsageRecorder) *Controller {
	c.resourceSource = source
	c.resourceRecorder = recorder
	return c
}

func (c *Controller) WriteResourceMetrics(w io.Writer) {
	if source, ok := c.resourceSource.(interface{ WriteMetrics(io.Writer) }); ok {
		source.WriteMetrics(w)
	}
}

type resourceSession struct {
	mu     sync.Mutex
	latest *wfv1.Workflow
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	runID  string
}

func (c *Controller) resourceSession(ctx context.Context, runID string) *resourceSession {
	if c.resourceSource == nil || c.resourceRecorder == nil || c.kube == nil {
		return nil
	}
	s := &resourceSession{wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), runID: runID}
	go func() {
		defer close(s.done)
		// An unrecovered panic in any goroutine terminates the Go server.
		// Telemetry failure must stop this session, not execution supervision.
		defer func() {
			if recover() != nil {
				log.Print("WARN: external resource sampler stopped after panic; usage remains incomplete")
			}
		}()
		finished := map[string]time.Time{}
		interval := c.resourceSource.Interval()
		if interval < time.Second {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		started := false
		warned := false
		sample := func(final bool) {
			wf := s.snapshot()
			if wf == nil {
				return
			}
			parent := ctx
			if final {
				parent = context.WithoutCancel(ctx)
			}
			sampleCtx, cancel := context.WithTimeout(parent, 5*time.Second)
			defer cancel()
			if err := c.sampleResourceUsageWithState(sampleCtx, wf, runID, finished); err != nil && !warned {
				log.Print("WARN: external step resource sampling incomplete; missing usage remains unknown")
				warned = true
			}
		}
		for {
			select {
			case <-ctx.Done():
				sample(true)
				return
			case <-s.stop:
				sample(true)
				return
			case <-s.wake:
				if !started {
					started = true
					sample(false)
				}
			case <-ticker.C:
				if started {
					sample(false)
				}
			}
		}
	}()
	return s
}
func (s *resourceSession) update(wf *wfv1.Workflow) {
	if s == nil || wf == nil || len(wf.Status.Nodes) > 2048 {
		return
	}
	s.mu.Lock()
	if s.latest != nil && s.latest.UID != wf.UID {
		s.mu.Unlock()
		return
	}
	// Workflow GET responses handed here are immutable: the controller replaces
	// the pointer on its next poll. Copy only on the slower sampling cadence.
	s.latest = wf
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *resourceSession) snapshot() *wfv1.Workflow {
	s.mu.Lock()
	wf := s.latest
	s.mu.Unlock()
	if wf == nil {
		return nil
	}
	return wf.DeepCopy()
}
func (s *resourceSession) finish() {
	if s == nil {
		return
	}
	close(s.stop)
	<-s.done
}

func (c *Controller) sampleResourceUsage(ctx context.Context, wf *wfv1.Workflow, runID string) error {
	return c.sampleResourceUsageWithState(ctx, wf, runID, nil)
}

func (c *Controller) sampleResourceUsageWithState(ctx context.Context, wf *wfv1.Workflow, runID string, finished map[string]time.Time) error {
	if c.verifyOwnership(wf, runID) != nil || wf.UID == "" || len(wf.Status.Nodes) > 2048 {
		return nil
	}
	pods, err := c.kube.CoreV1().Pods(wf.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=" + wf.Name, Limit: 2048})
	if err != nil {
		return err
	}
	byName := make(map[string]int, min(len(pods.Items), 2048))
	for i := range pods.Items[:min(len(pods.Items), 2048)] {
		byName[pods.Items[i].Name] = i
	}
	workflowSize := observedResourceWorkflowSize(wf)
	var fragmentLock argoworkflow.Lock
	_ = json.Unmarshal([]byte(wf.Annotations[FragmentsAnnotation]), &fragmentLock)
	bindings := map[string]resourcesampler.Binding{}
	candidates := 0
	snapshots := map[string]resourcesampler.Snapshot{}
	batch := make([]model.StepResourceUsage, 0, 256)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := c.resourceRecorder.RecordStepResourceUsage(ctx, batch)
		if err == nil {
			for _, value := range batch {
				key := resourceContainerKey(value.PodUID, value.ContainerID)
				_, known := finished[key]
				if finished != nil && (known || len(finished) < 4096) && value.Terminal && !value.ContainerFinishedAt.IsZero() {
					finished[key] = value.PodFinishedAt
				}
			}
			batch = batch[:0]
		}
		return err
	}
	appendValues := func(values []model.StepResourceUsage) error {
		for _, value := range values {
			if resourceContainerRecorded(resourceContainerKey(value.PodUID, value.ContainerID), value.PodFinishedAt, finished) {
				continue
			}
			value.WorkflowSize = workflowSize
			batch = append(batch, value)
			if len(batch) == 256 {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		return nil
	}
	getSnapshot := func(node string) resourcesampler.Snapshot {
		snapshot, found := snapshots[node]
		if !found {
			snapshot, _ = c.resourceSource.Snapshot(ctx, node)
			snapshot = resourcesampler.IndexSnapshot(snapshot)
			snapshots[node] = snapshot
		}
		return snapshot
	}
	for _, step := range stepAttempts(wf) {
		if err := ctx.Err(); err != nil {
			return err
		}
		at, ok := byName[step.PodName]
		if !ok {
			continue
		}
		pod := &pods.Items[at]
		node, ok := wf.Status.Nodes[step.NodeID]
		if !ok || node.ID != step.NodeID || step.AttemptAmbiguous || node.Type != wfv1.NodeTypePod {
			continue
		}
		// Validate ownership before requesting even a node scrape. Observe repeats
		// all bindings and verifies each current container/cgroup incarnation.
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.APIVersion != "argoproj.io/v1alpha1" || owner.Kind != "Workflow" || owner.UID != wf.UID || owner.Name != wf.Name || pod.Namespace != wf.Namespace || pod.Annotations["workflows.argoproj.io/node-id"] != node.ID || pod.Annotations["workflows.argoproj.io/node-name"] != node.Name || pod.Spec.NodeName == "" {
			continue
		}
		binding := resourcesampler.Binding{RunID: runID, WorkflowName: wf.Name, WorkflowUID: string(wf.UID), Namespace: wf.Namespace, NodeID: node.ID, NodeName: node.Name, PodName: step.PodName, Burn: step.Burn, Step: step.Step}
		binding.TemplateName = node.TemplateName
		binding.FragmentRepository, _ = argoworkflow.FragmentTemplateOwner(node.TemplateName, fragmentLock)
		bindings[string(pod.UID)] = binding
		pending := resourceContainerCandidates(pod, finished)
		if pending == 0 {
			continue
		}
		candidates += pending
		if candidates > 4096 {
			break
		}
		if err := appendValues(resourcesampler.ObserveContainers(binding, pod, getSnapshot(pod.Spec.NodeName), time.Now().UTC())); err != nil {
			return err
		}
	}
	if err := flush(); err != nil {
		return err
	}
	// Only workflows declaring VM resources need the two additional read-only
	// inventories. Never discover ownership from launcher metric labels.
	needsVM := false
	for _, t := range wf.Spec.Templates {
		if t.Resource != nil && strings.Contains(t.Resource.Manifest, "VirtualMachineInstance") {
			needsVM = true
			break
		}
	}
	if !needsVM || c.vmi == nil || candidates >= 4096 {
		return nil
	}
	vmis, err := c.vmi.Resource(vmiResource).Namespace(wf.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "oberth.ci/workflow=" + wf.Name, Limit: 2048})
	if err != nil {
		return err
	}
	type launcherBinding struct {
		binding   resourcesampler.Binding
		name, uid string
	}
	owned := map[string]launcherBinding{}
	for _, vmi := range vmis.Items[:min(len(vmis.Items), 2048)] {
		owner := metav1.GetControllerOf(&vmi)
		if owner == nil || owner.APIVersion != "argoproj.io/v1alpha1" || owner.Kind != "Workflow" || owner.Name != wf.Name || owner.UID != wf.UID {
			continue
		}
		if vmi.GetAPIVersion() != "kubevirt.io/v1" || vmi.GetKind() != "VirtualMachineInstance" || vmi.GetUID() == "" || !workflowOwnsResource(wf, &vmi) {
			continue
		}
		binding, ok := bindings[vmi.GetAnnotations()[vmiOperationAnnotation]]
		if ok {
			owned[string(vmi.GetUID())] = launcherBinding{binding, vmi.GetName(), string(vmi.GetUID())}
		}
	}
	if len(owned) == 0 {
		return nil
	}
	launchers, err := c.kube.CoreV1().Pods(wf.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "kubevirt.io=virt-launcher", Limit: 2048})
	if err != nil {
		return err
	}
	for _, pod := range launchers.Items[:min(len(launchers.Items), 2048)] {
		if err := ctx.Err(); err != nil {
			return err
		}
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || pod.Namespace != wf.Namespace || pod.Spec.NodeName == "" {
			continue
		}
		parent, ok := owned[string(owner.UID)]
		if !ok || owner.APIVersion != "kubevirt.io/v1" || owner.Kind != "VirtualMachineInstance" || owner.Name != parent.name {
			continue
		}
		pending := resourceContainerCandidates(&pod, finished)
		if pending == 0 {
			continue
		}
		candidates += pending
		if candidates > 4096 {
			break
		}
		binding := parent.binding
		binding.PodName = pod.Name
		binding.LauncherOwnerName, binding.LauncherOwnerUID = parent.name, parent.uid
		if err := appendValues(resourcesampler.ObserveLauncher(binding, &pod, getSnapshot(pod.Spec.NodeName), time.Now().UTC())); err != nil {
			return err
		}
	}
	return errors.Join(flush(), ctx.Err())
}

// Read only the verified Workflow's existing declaration and server-bound
// trigger parameter. Never use progress/termination messages as class evidence.
// A size-by-trigger declaration without an unambiguous bound trigger is unknown.
func observedResourceWorkflowSize(wf *wfv1.Workflow) periapsis.Size {
	byTrigger, err := argoworkflow.DeclaredSizesByTrigger(wf)
	if err != nil {
		return ""
	}
	if len(byTrigger) == 0 {
		size, err := argoworkflow.DeclaredSize(wf)
		if err != nil {
			return ""
		}
		return size
	}
	var trigger argoworkflow.RunTrigger
	seen := false
	for _, parameter := range wf.Spec.Arguments.Parameters {
		if parameter.Name != argoworkflow.TriggerParameter {
			continue
		}
		if seen || parameter.Value == nil || parameter.ValueFrom != nil {
			return ""
		}
		seen = true
		trigger = argoworkflow.RunTrigger(parameter.Value.String())
	}
	if !seen || !trigger.Valid() {
		return ""
	}
	size, err := argoworkflow.DeclaredSizeFor(wf, trigger)
	if err != nil {
		return ""
	}
	return size
}

func resourceContainerKey(podUID, containerID string) string { return podUID + "\x00" + containerID }

func resourceContainerCandidates(pod *corev1.Pod, finished map[string]time.Time) int {
	n := 0
	podFinished := resourcesampler.PodFinishedAt(pod)
	for _, group := range [][]corev1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses} {
		for _, status := range group {
			if status.ContainerID == "" || (status.State.Running == nil && status.State.Terminated == nil) {
				continue
			}
			if !resourceContainerRecorded(resourceContainerKey(string(pod.UID), status.ContainerID), podFinished, finished) {
				n++
			}
		}
	}
	return n
}

// A finish can arrive one poll after the containers terminate. Re-observe that
// transition once, including when the bounded container cache is already full.
func resourceContainerRecorded(key string, podFinished time.Time, finished map[string]time.Time) bool {
	previous, ok := finished[key]
	return ok && (podFinished.IsZero() || !previous.IsZero())
}
