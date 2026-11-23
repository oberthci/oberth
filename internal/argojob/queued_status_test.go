package argojob_test

import (
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func queuedPlan() admissioncontract.Plan {
	return admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build"}, {Path: "test", Name: "test"}}}
}

func waitingNode(id, path, lock string) wf.NodeStatus {
	n := nativeNode(id, path, wf.NodePending, 1)
	n.SynchronizationStatus = &wf.NodeSynchronizationStatus{Waiting: lock}
	return n
}

// A Running Workflow none of whose step nodes has started is a queued run; it
// runs from the first step node that runs or ends.
func TestRunIsQueuedUntilAStepNodeStarts(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		phases []wf.NodePhase
		status stepmodel.RunStatus
		reason string
	}{
		"all pending":   {[]wf.NodePhase{wf.NodePending, wf.NodePending}, stepmodel.RunQueued, "waiting to start"},
		"one pending":   {[]wf.NodePhase{wf.NodePending}, stepmodel.RunQueued, "waiting to start"},
		"one running":   {[]wf.NodePhase{wf.NodeRunning, wf.NodePending}, stepmodel.RunRunning, ""},
		"one finished":  {[]wf.NodePhase{wf.NodeSucceeded, wf.NodePending}, stepmodel.RunRunning, ""},
		"one skipped":   {[]wf.NodePhase{wf.NodePending, wf.NodeSkipped}, stepmodel.RunQueued, "waiting to start"},
		"one failed":    {[]wf.NodePhase{wf.NodeFailed}, stepmodel.RunRunning, ""},
		"no step nodes": {nil, stepmodel.RunQueued, "waiting to start"},
	} {
		w := nativeWorkflow()
		for i, phase := range c.phases {
			n := nativeNode(string(rune('a'+i)), "."+queuedPlan().Entries[i].Path, phase, 1)
			w.Status.Nodes[n.ID] = n
		}
		out := project(t, argojob.Projection{Plan: queuedPlan(), Workflow: w})
		if out.Status != c.status || out.Reason != c.reason {
			t.Fatalf("%s: %s %q, want %s %q", name, out.Status, out.Reason, c.status, c.reason)
		}
	}
}

// A retry waiting again after an attempt ended keeps the run running.
func TestRetryWaitingAgainKeepsTheRunRunning(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	retry := wf.NodeStatus{ID: "retry", Name: w.Name + ".build", Type: wf.NodeTypeRetry, Phase: wf.NodeRunning, Children: []string{"one", "two"}}
	one, two := nativeNode("one", ".build(0)", wf.NodeFailed, 1), nativeNode("two", ".build(1)", wf.NodePending, 2)
	w.Status.Nodes = wf.Nodes{"retry": retry, "one": one, "two": two}
	out := project(t, argojob.Projection{Plan: queuedPlan(), Workflow: w})
	if out.Status != stepmodel.RunRunning || out.Steps[0].Status != stepmodel.StepQueued {
		t.Fatalf("retry wait: %s %+v", out.Status, out.Steps[0])
	}
}

// A queued run names the lock its first waiting step waits for, whether the
// waiting node is the step's Pod or the structural node of a retried template
// before any Pod exists; the step names it too.
func TestQueuedRunNamesTheLockItWaitsFor(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		node   func(w *wf.Workflow) []wf.NodeStatus
		reason string
		step   int
	}{
		"pod on a sync limit": {func(*wf.Workflow) []wf.NodeStatus {
			return []wf.NodeStatus{waitingNode("a", ".build", "ci/ConfigMap/oberth-sync-limits/go")}
		}, "waiting for sync limit go", 0},
		"retry node on a sync limit": {func(w *wf.Workflow) []wf.NodeStatus {
			return []wf.NodeStatus{{ID: "r", Name: w.Name + ".test", Type: wf.NodeTypeRetry, Phase: wf.NodePending, SynchronizationStatus: &wf.NodeSynchronizationStatus{Waiting: "ci/ConfigMap/oberth-sync-limits/heavy-go"}}}
		}, "waiting for sync limit heavy-go", 1},
		"mutex": {func(*wf.Workflow) []wf.NodeStatus {
			return []wf.NodeStatus{waitingNode("a", ".build", "ci/Mutex/release-finalize")}
		}, "waiting for mutex release-finalize", 0},
		"name too long": {func(*wf.Workflow) []wf.NodeStatus {
			return []wf.NodeStatus{waitingNode("a", ".build", "ci/ConfigMap/oberth-sync-limits/"+strings.Repeat("k", 30))}
		}, "waiting for a lock", 0},
		"database lock": {func(*wf.Workflow) []wf.NodeStatus {
			return []wf.NodeStatus{waitingNode("a", ".build", "ci/Database/shared")}
		}, "waiting for a lock", 0},
	} {
		w := nativeWorkflow()
		for _, n := range c.node(w) {
			w.Status.Nodes[n.ID] = n
		}
		out := project(t, argojob.Projection{Plan: queuedPlan(), Workflow: w})
		waiting := out.Steps[c.step]
		if out.Status != stepmodel.RunQueued || out.Reason != c.reason || waiting.DisplayReason != c.reason || !strings.Contains(waiting.Detail, "oberth lock status") {
			t.Fatalf("%s: run %s %q, step %+v", name, out.Status, out.Reason, waiting)
		}
	}
}

// A Workflow the controller keeps Pending for the run cap is a queued run
// waiting for a run slot, with the native message as detail; a Pending
// Workflow without that message is waiting to start.
func TestPendingWorkflowWaitsForARunSlot(t *testing.T) {
	t.Parallel()
	const throttled = "Workflow processing has been postponed because too many workflows are already running"
	for message, reason := range map[string]string{throttled: "waiting for a run slot", "Waiting for a PVC to be created.": "waiting to start", "": "waiting to start"} {
		w := nativeWorkflow()
		w.Status.Phase, w.Status.Message = wf.WorkflowPending, message
		out := project(t, argojob.Projection{Plan: queuedPlan(), Workflow: w})
		if out.Status != stepmodel.RunQueued || out.Reason != reason || out.Detail != message || out.Steps[0].DisplayReason != reason {
			t.Fatalf("%q: %s %q detail %q step %q", message, out.Status, out.Reason, out.Detail, out.Steps[0].DisplayReason)
		}
		if reason == "waiting for a run slot" && out.Steps[0].Detail != message {
			t.Fatalf("run slot step detail %q", out.Steps[0].Detail)
		}
	}
}

// A Workflow pending on its own lock names it.
func TestWorkflowLevelLockNamesTheMutex(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowPending
	w.Status.Synchronization = &wf.SynchronizationStatus{Mutex: &wf.MutexStatus{Waiting: []wf.MutexHolding{{Mutex: "ci/Mutex/release", Holder: "ci/other"}}}}
	out := project(t, argojob.Projection{Plan: queuedPlan(), Workflow: w})
	if out.Status != stepmodel.RunQueued || out.Reason != "waiting for mutex release" {
		t.Fatalf("workflow lock: %s %q", out.Status, out.Reason)
	}
}
