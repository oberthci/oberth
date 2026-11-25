package argojob

import (
	"context"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

// DeliveryIdentity derives a receipt key from a live bound Pod and its native
// Workflow node. Request selectors and container contents supply no identity.
func DeliveryIdentity(w *wf.Workflow, p *core.Pod) (runlog.InfrastructureReceipt, error) {
	if w == nil || p == nil || terminalWorkflow(w) || w.Labels[nativebinding.EngineLabel] != nativebinding.EngineValue {
		return runlog.InfrastructureReceipt{}, ownership(nil)
	}
	n, ok := w.Status.Nodes[p.Annotations["workflows.argoproj.io/node-id"]]
	id := deliveryRunID(w)
	if !ok || n.Type != wf.NodeTypePod || !podOwned(w, n, p) || id == "" {
		return runlog.InfrastructureReceipt{}, ownership(nil)
	}
	path, known := indexPaths(w)[n.ID]
	if !known {
		return runlog.InfrastructureReceipt{}, ownership(nil)
	}
	return runlog.InfrastructureReceipt{RunID: id, WorkflowUID: string(w.UID), NodeID: n.ID, Step: path.name, PodUID: string(p.UID), PodRestarts: n.FailedPodRestarts}, nil
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
			return Observation{}, err
		}
	}
	return Project(ctx, in)
}

func receiptDiagnosis(d diagnosis, in projectionInput, n wf.NodeStatus, p *core.Pod) diagnosis {
	if !terminalNode(n) || d.class == stepmodel.FailureCancelled || d.class == stepmodel.FailureDeadline {
		return d
	}
	podUID := deliveryPodUID(n, in, p)
	if podUID == "" {
		return d
	}
	for _, r := range in.infrastructure {
		if !matchesDeliveryReceipt(r, in, n, podUID) {
			continue
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
