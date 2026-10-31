package argojob

import (
	"context"
	"io"
	"log"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
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
			if err := c.sampleResourceUsage(sampleCtx, wf, runID); err != nil && !warned {
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
	snapshots := map[string]resourcesampler.Snapshot{}
	batch := make([]model.StepResourceUsage, 0, 256)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := c.resourceRecorder.RecordStepResourceUsage(ctx, batch)
		batch = batch[:0]
		return err
	}
	for _, step := range stepAttempts(wf) {
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
		// all bindings and verifies the current main container/cgroup incarnation.
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.APIVersion != "argoproj.io/v1alpha1" || owner.Kind != "Workflow" || owner.UID != wf.UID || owner.Name != wf.Name || pod.Namespace != wf.Namespace || pod.Annotations["workflows.argoproj.io/node-id"] != node.ID || pod.Annotations["workflows.argoproj.io/node-name"] != node.Name || pod.Spec.NodeName == "" {
			continue
		}
		snapshot, found := snapshots[pod.Spec.NodeName]
		if !found {
			snapshot, _ = c.resourceSource.Snapshot(ctx, pod.Spec.NodeName)
			snapshots[pod.Spec.NodeName] = snapshot
		}
		value, ok := resourcesampler.Observe(resourcesampler.Binding{RunID: runID, WorkflowName: wf.Name, WorkflowUID: string(wf.UID), Namespace: wf.Namespace, NodeID: node.ID, NodeName: node.Name, PodName: step.PodName, Burn: step.Burn, Step: step.Step}, pod, snapshot, time.Now().UTC())
		if !ok {
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
	return flush()
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
