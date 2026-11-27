package argojob_test

import (
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestQuotaHasPositiveNativeAdmissionEvidence(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		phase           wf.NodePhase
		message, reason string
	}{
		{wf.NodePending, "pods is forbidden: exceeded quota: private-SENTINEL", "waiting for quota"},
		{wf.NodeFailed, "pods is forbidden: exceeded quota: private-SENTINEL", "quota exceeded"},
		{wf.NodeFailed, "pods is forbidden: unavailable policy", "Pod refused by the cluster"},
	} {
		t.Run(string(c.phase)+c.reason, func(t *testing.T) {
			w := nativeWorkflow()
			n := nativeNode("one", ".build", c.phase, 1)
			n.Message = c.message
			w.Status.Nodes[n.ID] = n
			if c.phase == wf.NodeFailed {
				w.Status.Phase = wf.WorkflowFailed
			}
			o := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}})
			if o.Steps[0].DisplayReason != c.reason {
				t.Fatal("quota diagnosis lost its native source", o)
			}
		})
	}
}

func TestInitImageWaitSurvivesCurrentNodePhase(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodePending, 1)
	w.Status.Nodes[n.ID] = n
	p := nativePod(w, n, 0)
	p.Status.Phase, p.Status.ContainerStatuses = core.PodPending, nil
	p.Status.InitContainerStatuses = []core.ContainerStatus{{Name: "authored-init", State: core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "private-SENTINEL"}}}}
	o := project(t, argojob.Projection{Workflow: w, Pods: []*core.Pod{p}, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}})
	if o.Reason != "image pull failed" || o.Steps[0].Diagnostic == nil || o.Steps[0].Diagnostic.Cause != "image" {
		t.Fatal("init-container wait was hidden", o)
	}
}

func TestPressureNeedsAnAffectedAssignedPod(t *testing.T) {
	t.Parallel()
	for _, c := range []pressureCase{
		{"affected", true, true, true, "node memory pressure"},
		{"cleared", true, false, true, "Pod evicted"},
		{"unrelated", true, true, false, "Pod evicted"},
		{"executing", false, true, true, "running"},
	} {
		t.Run(c.name, func(t *testing.T) { checkPressureFact(t, c) })
	}
}

type pressureCase struct {
	name                         string
	affected, pressure, assigned bool
	reason                       string
}

func checkPressureFact(t *testing.T, c pressureCase) {
	t.Helper()
	w := nativeWorkflow()
	phase := wf.NodeRunning
	if c.affected {
		phase = wf.NodeFailed
		w.Status.Phase = wf.WorkflowFailed
	}
	n := nativeNode("one", ".build", phase, 1)
	w.Status.Nodes[n.ID] = n
	p := nativePod(w, n, 1)
	p.Spec.NodeName = "own-node"
	if c.affected {
		p.Status.Reason, p.Status.Message = "Evicted", "The node was low on resource: memory."
	} else {
		p.Status.Phase = core.PodRunning
		p.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
	}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "other-node", UID: "native-node-uid"}}
	if c.assigned {
		node.Name = p.Spec.NodeName
	}
	status := core.ConditionFalse
	if c.pressure {
		status = core.ConditionTrue
	}
	node.Status.Conditions = []core.NodeCondition{{Type: core.NodeMemoryPressure, Status: status, LastTransitionTime: meta.NewTime(time.Unix(1, 0))}}
	o := project(t, argojob.Projection{Workflow: w, Pods: []*core.Pod{p}, Nodes: []*core.Node{node}, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}})
	assertPressureFact(t, c, o)
}

func assertPressureFact(t *testing.T, c pressureCase, o argojob.Observation) {
	t.Helper()
	if o.Steps[0].DisplayReason != c.reason {
		t.Fatal("pressure escaped its assigned native evidence", o)
	}
	if c.affected && c.pressure && c.assigned && (o.Steps[0].Diagnostic == nil || o.Steps[0].Diagnostic.NativeUID != "native-node-uid") {
		t.Fatal("pressure lost Node provenance", o)
	}
	if !c.affected && o.Status != stepmodel.RunRunning {
		t.Fatal("pressure changed the native status", o)
	}
}
