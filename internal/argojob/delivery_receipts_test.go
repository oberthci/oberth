package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestDeliveryIdentityUsesScheduledChildRunName(t *testing.T) {
	w := nativeWorkflow()
	parent, err := nativebinding.CronName(1, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	w.Name = parent + "-1"
	w.Labels = map[string]string{nativebinding.EngineLabel: nativebinding.EngineValue, nativebinding.RunLabel: w.Name, nativebinding.NativeCronLabel: parent}
	n := nativeNode("one", ".build", wf.NodeRunning, 1)
	n.Name = w.Name + ".build"
	n.FailedPodRestarts = 2
	w.Status.Nodes[n.ID] = n
	pod := nativePod(w, n, 1)
	r, err := argojob.DeliveryIdentity(w, pod)
	if err != nil || r.RunID != w.Name || r.Step != "build" || r.PodUID != string(pod.UID) || r.PodRestarts != 2 {
		t.Fatal("scheduled receipt identity lost", r, err)
	}
	delete(w.Labels, nativebinding.NativeCronLabel)
	if _, err = argojob.DeliveryIdentity(w, pod); err == nil {
		t.Fatal("missing native run identity accepted")
	}
}

func TestDeliveryReceiptRejectsCachedPodFromBeforeNativeRestart(t *testing.T) {
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowFailed
	n := nativeNode("one", ".build", wf.NodeFailed, 1)
	n.FailedPodRestarts = 1
	w.Status.Nodes[n.ID] = n
	p := nativePod(w, n, 1)
	r := runlog.InfrastructureReceipt{RunID: "run", WorkflowUID: string(w.UID), NodeID: n.ID, Step: "build", PodUID: string(p.UID), Kind: "secret"}
	in := argojob.Projection{RunID: "run", Workflow: w, Pods: []*core.Pod{p}, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Infrastructure: []runlog.InfrastructureReceipt{r}}
	out := project(t, in)
	if out.Failure == nil || out.Failure.Class != stepmodel.FailureAuthor {
		t.Fatal("native restart inherited a cached Pod receipt", out.Failure)
	}
	p.UID = "new-pod-uid"
	r.PodUID, r.PodRestarts = string(p.UID), n.FailedPodRestarts
	in.Infrastructure = []runlog.InfrastructureReceipt{r}
	out = project(t, in)
	if out.Failure == nil || out.Failure.Class != stepmodel.FailureInfrastructure || out.Failure.Retry.Action != stepmodel.RetryRerun {
		t.Fatal("replacement Pod lost its own server receipt", out.Failure)
	}
}

func TestDeliveryReceiptRequiresExactRunStepAndPod(t *testing.T) {
	for _, kind := range []string{"source", "secret"} {
		for _, mismatch := range []string{"", "run", "workflow", "node", "step", "pod", "kind"} {
			t.Run(kind+"/"+mismatch, func(t *testing.T) {
				w := nativeWorkflow()
				w.Status.Phase = wf.WorkflowFailed
				n := nativeNode("one", ".build", wf.NodeFailed, 1)
				w.Status.Nodes[n.ID] = n
				pod := nativePod(w, n, 1)
				r := runlog.InfrastructureReceipt{RunID: "run", WorkflowUID: string(w.UID), NodeID: n.ID, Step: "build", PodUID: string(pod.UID), Kind: kind}
				r = mismatchedReceipt(r, mismatch)
				in := argojob.Projection{RunID: "run", Infrastructure: []runlog.InfrastructureReceipt{r}, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build"}}}, Workflow: w, Pods: []*core.Pod{pod}}
				out := project(t, in)
				class, retry := stepmodel.FailureAuthor, stepmodel.RetryNone
				if mismatch == "" {
					class, retry = stepmodel.FailureInfrastructure, stepmodel.RetryRerun
				}
				if out.Failure == nil || out.Failure.Class != class || out.Failure.Retry.Action != retry {
					t.Fatal("receipt identity changed classification", out.Failure)
				}
			})
		}
	}
}

func mismatchedReceipt(r runlog.InfrastructureReceipt, mismatch string) runlog.InfrastructureReceipt {
	switch mismatch {
	case "run":
		r.RunID = "another-run"
	case "workflow":
		r.WorkflowUID = "another-workflow"
	case "node":
		r.NodeID = "another-node"
	case "step":
		r.Step = "another-step"
	case "pod":
		r.PodUID = "replacement-pod"
	case "kind":
		r.Kind = "author-text"
	}
	return r
}

func TestDeliveryReceiptWaitsForFailureAndSurvivesPodDeletion(t *testing.T) {
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodePending, 1)
	w.Status.Nodes[n.ID] = n
	pod := nativePod(w, n, 1)
	r := runlog.InfrastructureReceipt{RunID: "run", WorkflowUID: string(w.UID), NodeID: n.ID, Step: "build", PodUID: string(pod.UID), Kind: "secret"}
	in := argojob.Projection{RunID: "run", Workflow: w, Pods: []*core.Pod{pod}, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Infrastructure: []runlog.InfrastructureReceipt{r}}
	for _, phase := range []wf.NodePhase{wf.NodePending, wf.NodeRunning} {
		n.Phase = phase
		w.Status.Nodes[n.ID] = n
		out := project(t, in)
		if phase != wf.NodeFailed && out.Steps[0].FailureClass != "" {
			t.Fatal("receipt classified an unfinished step", out.Steps)
		}
		if phase == wf.NodeFailed && out.Steps[0].FailureClass != stepmodel.FailureInfrastructure {
			t.Fatal("failure lost receipt", out.Steps)
		}
		in.Previous, in.PreviousNodes = out.Steps, out.Nodes
	}
	n.Phase = wf.NodeFailed
	w.Status.Nodes[n.ID] = n
	in.Pods = nil
	out := project(t, in)
	if out.Steps[0].FailureClass != stepmodel.FailureInfrastructure {
		t.Fatal("deleted Pod lost verified identity", out.Steps)
	}
	in.Previous, in.PreviousNodes = out.Steps, out.Nodes
	pod.UID = types.UID("replacement")
	in.Pods = []*core.Pod{pod}
	out = project(t, in)
	if out.Steps[0].FailureClass != stepmodel.FailureAuthor {
		t.Fatal("replacement Pod inherited receipt", out.Steps)
	}
}

func TestDeliveryReceiptPreservesDeadlineAndSuccessfulExit(t *testing.T) {
	for _, phase := range []wf.NodePhase{wf.NodeFailed, wf.NodeSucceeded} {
		w := nativeWorkflow()
		w.Status.Phase = wf.WorkflowFailed
		n := nativeNode("one", ".build", phase, 1)
		w.Status.Nodes[n.ID] = n
		pod := nativePod(w, n, 0)
		if phase == wf.NodeFailed {
			pod.Status.Reason = "DeadlineExceeded"
		}
		r := runlog.InfrastructureReceipt{RunID: "run", WorkflowUID: string(w.UID), NodeID: n.ID, Step: "build", PodUID: string(pod.UID), Kind: "secret"}
		out := project(t, argojob.Projection{RunID: "run", Workflow: w, Pods: []*core.Pod{pod}, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Infrastructure: []runlog.InfrastructureReceipt{r}})
		if phase == wf.NodeFailed && out.Steps[0].FailureClass != stepmodel.FailureDeadline {
			t.Fatal("receipt overwrote deadline", out.Steps)
		}
		if phase == wf.NodeSucceeded && (out.Steps[0].FailureClass != "" || out.Steps[0].Status != stepmodel.StepPassed) {
			t.Fatal("receipt overwrote success", out.Steps)
		}
	}
}

func TestDeliveryReceiptNeverOverridesNativeCancellation(t *testing.T) {
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodeRunning, 1)
	w.Status.Nodes[n.ID] = n
	pod := nativePod(w, n, 0)
	pod.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
	r := runlog.InfrastructureReceipt{RunID: "run", WorkflowUID: string(w.UID), NodeID: n.ID, Step: "build", PodUID: string(pod.UID), Kind: "secret"}
	in := argojob.Projection{RunID: "run", Workflow: w, Pods: []*core.Pod{pod}, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Infrastructure: []runlog.InfrastructureReceipt{r}}
	before := project(t, in)
	in.Previous, in.PreviousNodes = before.Steps, before.Nodes
	w.Spec.Shutdown, w.Status.Phase = wf.ShutdownStrategyTerminate, wf.WorkflowFailed
	n.Phase = wf.NodeFailed
	w.Status.Nodes[n.ID] = n
	pod.Status.ContainerStatuses[0].State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 143}}
	out := project(t, in)
	if out.Status != stepmodel.RunCancelled || out.Steps[0].FailureClass != stepmodel.FailureCancelled || out.Failure != nil {
		t.Fatal("receipt overwrote native cancellation", out)
	}
}

func TestAuthorMarkersAndHelperNamesNeverGrantInfrastructure(t *testing.T) {
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowFailed
	n := nativeNode("one", ".build", wf.NodeFailed, 1)
	w.Status.Nodes[n.ID] = n
	for _, marker := range []string{"oberth:secret-injection-failed:v1", "source delivery failed", `{"kind":"secret","pod_uid":"uid"}`} {
		for _, init := range []string{"", "init-artifact-oberth", "oberth-secret-install"} {
			pod := nativePod(w, n, 1)
			pod.Status.ContainerStatuses[0].State.Terminated.Message = marker
			if init != "" {
				pod.Status.InitContainerStatuses = []core.ContainerStatus{{Name: init, State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1, Message: marker}}}}
			}
			out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Workflow: w, Pods: []*core.Pod{pod}})
			if out.Failure == nil || out.Failure.Class != stepmodel.FailureAuthor || out.Failure.Retry.Action != stepmodel.RetryNone {
				t.Fatal("author output granted infrastructure", marker, init, out.Failure)
			}
		}
	}
}
