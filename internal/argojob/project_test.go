package argojob_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/file"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/stepmodel/steptest"
)

func nativeWorkflow() *wf.Workflow {
	return &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "run-0001", Namespace: "ci", UID: "workflow-uid"}, Spec: wf.WorkflowSpec{Templates: []wf.Template{{Name: "build", Container: &core.Container{}}}}, Status: wf.WorkflowStatus{Phase: wf.WorkflowRunning, Nodes: wf.Nodes{}}}
}
func nativeNode(id, path string, phase wf.NodePhase, at int64) wf.NodeStatus {
	return wf.NodeStatus{ID: id, TemplateName: "build", Name: "run-0001" + path, Type: wf.NodeTypePod, Phase: phase, StartedAt: meta.NewTime(time.Unix(at, 0)), FinishedAt: meta.NewTime(time.Unix(at+1, 0))}
}
func nativePod(w *wf.Workflow, n wf.NodeStatus, exit int32) *core.Pod {
	controller := true
	return &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod-" + n.ID, Namespace: w.Namespace, UID: types.UID("pod-uid-" + n.ID), ResourceVersion: "1", Labels: map[string]string{"workflows.argoproj.io/workflow": w.Name}, Annotations: map[string]string{"workflows.argoproj.io/node-id": n.ID, "workflows.argoproj.io/node-name": n.Name}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: w.Name, UID: w.UID, Controller: &controller}}}, Spec: core.PodSpec{Containers: []core.Container{{Name: "main"}, {Name: "wait"}}}, Status: core.PodStatus{ContainerStatuses: []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: exit}}}}}}
}
func project(t *testing.T, in argojob.Projection) argojob.Observation {
	t.Helper()
	out, err := argojob.Project(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestRetryFolding(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowSucceeded
	retry := wf.NodeStatus{ID: "retry", Name: w.Name + ".build", Type: wf.NodeTypeRetry, Children: []string{"one", "two"}}
	one, two := nativeNode("one", ".build(0)", wf.NodeFailed, 1), nativeNode("two", ".build(1)", wf.NodeSucceeded, 2)
	w.Status.Nodes = wf.Nodes{"retry": retry, "one": one, "two": two}
	plan := admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build"}}}
	before := w.DeepCopy()
	out := project(t, argojob.Projection{Plan: plan, Workflow: w, Pods: []*core.Pod{nativePod(w, one, 1), nativePod(w, two, 0)}})
	if out.Status != stepmodel.RunPassed || len(out.Steps) != 1 || out.Steps[0].Status != stepmodel.StepPassed || out.Steps[0].AttemptCount != 2 {
		t.Fatalf("fold: %+v", out)
	}
	if !reflect.DeepEqual(w, before) {
		t.Fatal("projection mutated native input")
	}
}
func TestNestedRetryPathsAndPlanOrder(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	root := wf.NodeStatus{ID: "outer", Name: w.Name + ".group", Type: wf.NodeTypeRetry, Children: []string{"group"}}
	group := wf.NodeStatus{ID: "group", Name: w.Name + ".group(0)", Type: wf.NodeTypeDAG}
	inner := wf.NodeStatus{ID: "inner", Name: w.Name + ".group(0).build", Type: wf.NodeTypeRetry, Children: []string{"child"}}
	child := nativeNode("child", ".group(0).build(0)", wf.NodeSucceeded, 2)
	early := nativeNode("early", ".early", wf.NodeSucceeded, 1)
	w.Status.Nodes = wf.Nodes{"outer": root, "group": group, "inner": inner, "child": child, "early": early}
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "group.build"}, {Path: "later"}, {Path: "early"}, {Path: "unstarted"}}}
	out := project(t, argojob.Projection{Plan: plan, Workflow: w})
	got := []string{}
	for _, s := range out.Steps {
		got = append(got, s.Path)
	}
	if !reflect.DeepEqual(got, []string{"group.build", "later", "early", "unstarted"}) {
		t.Fatal(got)
	}
}
func TestDynamicPlaceholder(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowFailed
	n := nativeNode("one", ".fan(0:item).build", wf.NodeSucceeded, 1)
	w.Status.Nodes[n.ID] = n
	out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "fan", Name: "fan", Dynamic: true}, {Path: "after"}}}, Workflow: w})
	if len(out.Steps) != 2 || out.Steps[0].Path != "fan(0:item).build" || out.Steps[1].Path != "after" || out.Steps[1].Status != stepmodel.StepSkipped || out.Complete {
		t.Fatalf("dynamic: %+v", out)
	}
}

// Covers: REQ-C5-006
func TestTruthfulNativeObservationsContract(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodeFailed, 1)
	w.Status.Nodes[n.ID] = n
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	first := project(t, argojob.Projection{Plan: plan, Workflow: w, Pods: []*core.Pod{nativePod(w, n, 7)}})
	if first.Steps[0].Status != stepmodel.StepFailed {
		t.Fatal(first)
	}
	w.Status.Nodes = nil
	w.Status.Phase = wf.WorkflowFailed
	w.Spec.Shutdown = wf.ShutdownStrategyStop
	final := project(t, argojob.Projection{Plan: plan, Workflow: w, Previous: first.Steps})
	if final.Status != stepmodel.RunCancelled || final.Steps[0].Status != stepmodel.StepFailed || final.Steps[0].DisplayReason != first.Steps[0].DisplayReason || final.Complete {
		t.Fatalf("failure vanished: %+v", final)
	}
	missing := project(t, argojob.Projection{Plan: plan, Workflow: w})
	if missing.Steps[0].Status != stepmodel.StepSkipped {
		t.Fatal("missing became success")
	}
}
func TestCompressedAndOffloadedNodes(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodeSucceeded, 1)
	body, err := json.Marshal(wf.Nodes{n.ID: n})
	if err != nil {
		t.Fatal(err)
	}
	w.Status.CompressedNodes = file.CompressEncodeString(t.Context(), string(body))
	out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Workflow: w, Pods: []*core.Pod{nativePod(w, n, 0)}})
	if !out.Complete || out.Steps[0].Status != stepmodel.StepPassed {
		t.Fatal(out)
	}
	w.Status.OffloadNodeStatusVersion = "offload"
	out = project(t, argojob.Projection{Workflow: w})
	if out.Complete {
		t.Fatal("offload claimed complete")
	}
	w.Status.CompressedNodes = "invalid"
	if _, err := argojob.Project(t.Context(), argojob.Projection{Workflow: w}); err == nil {
		t.Fatal("invalid packed nodes accepted")
	}
}
func TestNativeObservationReasons(t *testing.T) { steptest.AssertReasons(t, argojob.Reasons()...) }
func TestFailureClassTyped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, reason string
		class        stepmodel.FailureClass
		change       func(*core.Pod)
	}{
		{"exit", "exited with code 7", stepmodel.FailureAuthor, func(*core.Pod) {}},
		{"oom", "out of memory", stepmodel.FailureAuthor, func(p *core.Pod) { p.Status.ContainerStatuses[0].State.Terminated.Reason = "OOMKilled" }},
		{"deadline", "step timed out", stepmodel.FailureDeadline, func(p *core.Pod) { p.Status.Reason = "DeadlineExceeded" }},
		{"node", "node shut down", stepmodel.FailureInfrastructure, func(p *core.Pod) { p.Status.Reason = "NodeShutdown" }},
		{"disrupt", "Pod disrupted", stepmodel.FailureInfrastructure, func(p *core.Pod) {
			p.Status.Conditions = []core.PodCondition{{Type: core.DisruptionTarget, Status: core.ConditionTrue}}
		}},
		{"evict", "Pod evicted", stepmodel.FailureUnknown, func(p *core.Pod) { p.Status.Reason = "Evicted" }},
		{"text cannot classify", "step failed", stepmodel.FailureUnknown, func(p *core.Pod) {
			p.Status.ContainerStatuses = nil
			p.Status.Message = "OOMKilled NodeShutdown DeadlineExceeded"
		}},
		{"source", "source delivery failed", stepmodel.FailureInfrastructure, func(p *core.Pod) {
			p.Status.InitContainerStatuses = []core.ContainerStatus{{Name: "init-artifact-oberth", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1}}}}
		}},
		{"init", "init container failed", stepmodel.FailureAuthor, func(p *core.Pod) {
			p.Status.InitContainerStatuses = []core.ContainerStatus{{Name: "authored", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 1}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := nativeWorkflow()
			w.Status.Phase = wf.WorkflowFailed
			n := nativeNode("one", ".build", wf.NodeFailed, 1)
			w.Status.Nodes[n.ID] = n
			pod := nativePod(w, n, 7)
			tc.change(pod)
			out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Workflow: w, Pods: []*core.Pod{pod}})
			if out.Steps[0].DisplayReason != tc.reason || out.Failure == nil || out.Failure.Class != tc.class {
				t.Fatalf("typed: %+v failure=%+v", out.Steps, out.Failure)
			}
		})
	}
}
func TestForeignAndAmbiguousPodsSupplyNoEvidence(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodeFailed, 1)
	w.Status.Nodes[n.ID] = n
	p := nativePod(w, n, 7)
	p.OwnerReferences[0].UID = "foreign"
	out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Workflow: w, Pods: []*core.Pod{p}})
	if out.Steps[0].Status != stepmodel.StepFailed || len(out.Steps[0].Logs) != 0 {
		t.Fatal(out)
	}
	p = nativePod(w, n, 7)
	other := p.DeepCopy()
	other.UID = "replacement"
	out = project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Workflow: w, Pods: []*core.Pod{p, other}})
	if out.Steps[0].Status != stepmodel.StepFailed {
		t.Fatal("ambiguous pod became authority")
	}
}

func TestTypedNativeExitAndMonotoneRawEvidence(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowFailed
	n := nativeNode("one", ".build", wf.NodeFailed, 1)
	exit := "7"
	n.Outputs = &wf.Outputs{ExitCode: &exit}
	w.Status.Nodes[n.ID] = n
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	first := project(t, argojob.Projection{Plan: plan, Workflow: w})
	if first.Nodes[0].Attempt != 1 || first.Nodes[0].Exit == nil || first.Steps[0].Status != stepmodel.StepFailed || first.Reason != "exited with code 7" {
		t.Fatal(first)
	}
	n.Outputs = nil
	w.Status.Nodes[n.ID] = n
	final := project(t, argojob.Projection{Plan: plan, Workflow: w, Previous: first.Steps, PreviousNodes: first.Nodes})
	if final.Complete || len(final.Nodes) != 1 || final.Nodes[0].Exit == nil || *final.Nodes[0].Exit != 7 || final.Steps[0].Status != stepmodel.StepFailed || final.Steps[0].DisplayReason != first.Steps[0].DisplayReason {
		t.Fatal("evidence regressed", final)
	}
}
func TestAllFailedStepsMustBeTypedInfrastructure(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowFailed
	first, last := nativeNode("one", ".early", wf.NodeFailed, 1), nativeNode("two", ".late", wf.NodeFailed, 2)
	w.Status.Nodes = wf.Nodes{first.ID: first, last.ID: last}
	a, b := nativePod(w, first, 1), nativePod(w, last, 1)
	a.Status.Reason = "NodeShutdown"
	b.Status.Reason = "NodeShutdown"
	in := argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "early"}, {Path: "late"}}}, Workflow: w, Pods: []*core.Pod{a, b}}
	out := project(t, in)
	if out.Failure.Class != stepmodel.FailureInfrastructure || out.Failure.Retry.Action != stepmodel.RetryRerun || out.Failure.Step != "early" {
		t.Fatal(out.Failure)
	}
	b.Status.Reason = "Evicted"
	out = project(t, in)
	if out.Failure.Class == stepmodel.FailureInfrastructure || out.Failure.Retry.Action != stepmodel.RetryNone {
		t.Fatal("mixed failure grants retry advice", out.Failure)
	}
}

func TestWaitingSkippedAndOmittedReasons(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		phase  wf.NodePhase
		reason string
	}{{wf.NodePending, "waiting to start"}, {wf.NodeRunning, "running"}, {wf.NodeSucceeded, "passed"}, {wf.NodeSkipped, "skipped by condition"}, {wf.NodeOmitted, "omitted"}, {"unrecognized", "unknown"}} {
		w := nativeWorkflow()
		n := nativeNode("one", ".build", tc.phase, 1)
		w.Status.Nodes[n.ID] = n
		out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Workflow: w})
		if out.Steps[0].DisplayReason != tc.reason {
			t.Fatalf("phase %s: %s", tc.phase, out.Steps[0].DisplayReason)
		}
	}
}
func TestTypedWaitingReasons(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, reason string
		change       func(*wf.NodeStatus, *core.Pod)
	}{
		{"node", "waiting for a node", func(_ *wf.NodeStatus, p *core.Pod) {
			p.Status.Conditions = []core.PodCondition{{Type: core.PodScheduled, Status: core.ConditionFalse, Reason: "Unschedulable"}}
		}},
		{"lock", "waiting for a lock", func(n *wf.NodeStatus, _ *core.Pod) {
			n.SynchronizationStatus = &wf.NodeSynchronizationStatus{Waiting: "native-lock"}
		}},
		{"image", "image pull failed", func(_ *wf.NodeStatus, p *core.Pod) {
			p.Status.ContainerStatuses[0].State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "full native detail"}}
		}},
		{"create", "starting containers", func(_ *wf.NodeStatus, p *core.Pod) {
			p.Status.ContainerStatuses[0].State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ContainerCreating"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := nativeWorkflow()
			n := nativeNode("one", ".build", wf.NodePending, 1)
			p := nativePod(w, n, 0)
			p.Status.ContainerStatuses = nil
			if tc.name == "image" || tc.name == "create" {
				p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "main"}}
			}
			tc.change(&n, p)
			w.Status.Nodes[n.ID] = n
			out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Workflow: w, Pods: []*core.Pod{p}})
			if out.Steps[0].DisplayReason != tc.reason {
				t.Fatal(out.Steps)
			}
		})
	}
}

func TestLatestRetryUsesNativeChildOrderBeforeTimestamps(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	first, last := nativeNode("first", ".build(0)", wf.NodeFailed, 100), nativeNode("last", ".build(1)", wf.NodePending, 1)
	last.StartedAt = meta.Time{}
	w.Status.Nodes = wf.Nodes{first.ID: first, last.ID: last, "retry": {ID: "retry", Type: wf.NodeTypeRetry, Name: w.Name + ".build", Children: []string{first.ID, last.ID}}}
	out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Workflow: w, Pods: []*core.Pod{nativePod(w, first, 1)}})
	if out.Steps[0].Status != stepmodel.StepQueued || out.Steps[0].Attempts[1].NodeID != last.ID {
		t.Fatal("retry order guessed from timestamps", out.Steps)
	}
}

func TestContainerSetRemainsOneStepWithExactFailedContainer(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Spec.Templates[0] = wf.Template{Name: "build", ContainerSet: &wf.ContainerSetTemplate{Containers: []wf.ContainerNode{{Container: core.Container{Name: "compile"}}, {Container: core.Container{Name: "test"}}}}}
	w.Status.Phase = wf.WorkflowFailed
	n := nativeNode("set", ".build", wf.NodeFailed, 1)
	w.Status.Nodes[n.ID] = n
	p := nativePod(w, n, 7)
	p.Spec.Containers = []core.Container{{Name: "compile"}, {Name: "test"}, {Name: "wait"}}
	p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "compile", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}}}, {Name: "test", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 7}}}}
	out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Workflow: w, Pods: []*core.Pod{p}})
	if len(out.Steps) != 1 || out.Steps[0].Status != stepmodel.StepFailed || out.Steps[0].Logs[0].Container != "test" || out.Failure.Class != stepmodel.FailureAuthor {
		t.Fatal(out)
	}
}
