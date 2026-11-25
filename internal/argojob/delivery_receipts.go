package argojob

import (
	"context"
	"errors"
	"slices"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrDeliveryNodePending marks a Pod controlled by the Workflow whose node is
// not yet in the persisted status. Argo saves the node in the pass that creates
// the Pod, so the caller answers retryably instead of refusing.
var ErrDeliveryNodePending = errors.New("delivery node is not saved yet")

// DeliveryIdentity derives a receipt key from a live bound Pod and its native
// Workflow node. Request selectors and container contents supply no identity.
func DeliveryIdentity(w *wf.Workflow, p *core.Pod) (runlog.InfrastructureReceipt, error) {
	if w == nil || p == nil || terminalWorkflow(w) || w.Labels[nativebinding.EngineLabel] != nativebinding.EngineValue {
		return runlog.InfrastructureReceipt{}, ownership(nil)
	}
	node := p.Annotations["workflows.argoproj.io/node-id"]
	n, ok := w.Status.Nodes[node]
	id := deliveryRunID(w)
	if !ok && nodePending(w, p, node, id) {
		return runlog.InfrastructureReceipt{}, unavailable(ErrDeliveryNodePending)
	}
	if !ok || n.Type != wf.NodeTypePod || !podOwned(w, n, p) || id == "" {
		return runlog.InfrastructureReceipt{}, ownership(nil)
	}
	path, known := indexPaths(w)[n.ID]
	if !known {
		return runlog.InfrastructureReceipt{}, ownership(nil)
	}
	return runlog.InfrastructureReceipt{RunID: id, WorkflowUID: string(w.UID), NodeID: n.ID, Step: path.name, PodUID: string(p.UID), PodRestarts: n.FailedPodRestarts}, nil
}

// nodePending reports an absent node named by a bound run's own Pod.
func nodePending(w *wf.Workflow, p *core.Pod, node, id string) bool {
	return node != "" && id != "" && controlledBy(w, p)
}

func deliveryRunID(w *wf.Workflow) string {
	id := w.Labels[nativebinding.RunLabel]
	if id == "" {
		return ""
	}
	if w.Annotations[nativebinding.RunIDAnnotation] == id {
		return id
	}
	if w.Annotations[nativebinding.RunIDAnnotation] == "" && w.Labels[nativebinding.NativeCronLabel] != "" && id == w.Name {
		return id
	}
	return ""
}

// InfrastructureStore reads only server-written facts, independently of logs.
type InfrastructureStore interface {
	Infrastructure(context.Context, string) ([]runlog.InfrastructureReceipt, error)
}

func (e *Executor) project(ctx context.Context, t *tracking, w *wf.Workflow, pods []*core.Pod) (Observation, error) {
	in := Projection{RunID: t.binding.RunID, Plan: t.plan, Workflow: w, Pods: pods, Previous: t.previous, PreviousNodes: t.previousNodes}
	if e.config.Infrastructure != nil {
		var err error
		in.Infrastructure, err = e.config.Infrastructure.Infrastructure(ctx, t.binding.RunID)
		if err != nil {
			in.Infrastructure, in.DeliveryEvidenceUnavailable = nil, true
		}
		if err != nil || slices.ContainsFunc(in.Infrastructure, func(r runlog.InfrastructureReceipt) bool { return r.Uncertain }) {
			e.config.Logger.ErrorContext(ctx, "Delivery evidence unavailable", "run", t.binding.RunID)
		}
		if e.config.Pods != nil && len(in.Infrastructure) > 0 {
			in.Pods, err = e.deliveryPods(ctx, w, pods, in.Infrastructure)
			if err != nil {
				return Observation{}, err
			}
		}
	}
	return Project(ctx, in)
}

// A controller can publish Failed before the Pod informer catches up. Source
// attribution needs the failed init, so confirm it live before freezing a run.
func (e *Executor) deliveryPods(ctx context.Context, w *wf.Workflow, pods []*core.Pod, receipts []runlog.InfrastructureReceipt) ([]*core.Pod, error) {
	fresh := slices.Clone(pods)
	for _, r := range receipts {
		for i, p := range fresh {
			if !sourceReceiptNeedsPod(r, w, p) {
				continue
			}
			live, err := e.config.Pods.Get(ctx, p.Name, meta.GetOptions{})
			if err != nil {
				return nil, err
			}
			if live == nil || live.UID != p.UID || !podOwned(w, w.Status.Nodes[r.NodeID], live) {
				return nil, ownership(nil)
			}
			fresh[i] = live
		}
	}
	return fresh, nil
}

func sourceReceiptNeedsPod(r runlog.InfrastructureReceipt, w *wf.Workflow, p *core.Pod) bool {
	if r.Kind != "source" || p == nil || r.PodUID != string(p.UID) || r.WorkflowUID != string(w.UID) {
		return false
	}
	n, ok := w.Status.Nodes[r.NodeID]
	return ok && p.Annotations["workflows.argoproj.io/node-id"] == n.ID && r.PodRestarts == n.FailedPodRestarts && (n.Phase == wf.NodeFailed || n.Phase == wf.NodeError) && deliveryFailedDuring(r, n, nil) && !deliveryFailedDuring(r, n, p)
}

func receiptDiagnosis(d diagnosis, in projectionInput, n wf.NodeStatus, p *core.Pod) diagnosis {
	if !deliveryFailureMayExplain(d, n) {
		return d
	}
	if in.deliveryEvidenceUnavailable {
		return uncertainDeliveryDiagnosis(d)
	}
	podUID := deliveryPodUID(n, in, p)
	if podUID == "" {
		return d
	}
	for _, r := range in.infrastructure {
		if !matchesDeliveryReceipt(r, in, n, podUID) || !deliveryFailedDuring(r, n, p) {
			continue
		}
		if r.Uncertain {
			return uncertainDeliveryDiagnosis(d)
		}
		switch r.Kind {
		case "source":
			d.reason = reasonSource
		case "secret":
			d.reason = reasonSecret
		default:
			continue
		}
		d.class = stepmodel.FailureInfrastructure
		return d
	}
	return d
}

func deliveryFailureMayExplain(d diagnosis, n wf.NodeStatus) bool {
	return (n.Phase == wf.NodeFailed || n.Phase == wf.NodeError) && d.class != stepmodel.FailureCancelled && d.class != stepmodel.FailureDeadline
}

func uncertainDeliveryDiagnosis(d diagnosis) diagnosis {
	if d.class == stepmodel.FailureAuthor || d.class == stepmodel.FailureUnknown {
		d.reason, d.class = reasonDeliveryEvidence, stepmodel.FailureUnknown
	}
	return d
}

// Native timestamps bind the terminal delivery to the failure it diagnoses.
// A source receipt additionally requires a failed source init: a later command
// failure cannot be blamed on checkout, even when it happens immediately.
func deliveryFailedDuring(r runlog.InfrastructureReceipt, n wf.NodeStatus, p *core.Pod) bool {
	if r.Delivered || r.StartedAt.IsZero() || r.FinishedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) {
		return false
	}
	finished := deliveryFailureTime(r.Kind, n, p)
	// Kubernetes metav1.Time on the wire has whole-second precision.
	return !finished.IsZero() && !finished.Before(r.StartedAt.Truncate(time.Second)) && !finished.After(r.FinishedAt.Add(10*time.Second))
}

func deliveryFailureTime(kind string, n wf.NodeStatus, p *core.Pod) time.Time {
	finished := n.FinishedAt.Time
	if p != nil {
		statuses, name := p.Status.ContainerStatuses, "main"
		if kind == "source" {
			statuses, name = p.Status.InitContainerStatuses, "init-artifact-oberth"
			finished = time.Time{}
		}
		for _, c := range statuses {
			if c.Name == name && c.State.Terminated != nil {
				if c.State.Terminated.ExitCode == 0 {
					return time.Time{}
				}
				finished = c.State.Terminated.FinishedAt.Time
				if finished.IsZero() {
					finished = n.FinishedAt.Time
				}
			}
		}
	}
	return finished
}

func deliveryPodUID(n wf.NodeStatus, in projectionInput, p *core.Pod) string {
	if p != nil {
		return string(p.UID)
	}
	for _, old := range in.previousNodes {
		if old.NodeID == n.ID && old.Path == in.paths[n.ID].name {
			return old.PodUID
		}
	}
	return ""
}

func matchesDeliveryReceipt(r runlog.InfrastructureReceipt, in projectionInput, n wf.NodeStatus, podUID string) bool {
	return r.RunID == in.runID && r.WorkflowUID == string(in.w.UID) && r.NodeID == n.ID && r.Step == in.paths[n.ID].name && r.PodUID == podUID && r.PodRestarts == n.FailedPodRestarts
}
