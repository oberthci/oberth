package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// next projects w as the observation after previous, as the executor does.
func next(t *testing.T, w *wf.Workflow, previous argojob.Observation) argojob.Observation {
	t.Helper()
	return project(t, argojob.Projection{Plan: queuedPlan(), Workflow: w, Previous: previous.Steps, PreviousNodes: previous.Nodes})
}

// A step's waiting detail lasts as long as the wait: a throttled run that
// starts, then fails before a step is reached, keeps no throttle message on
// that step, running or finished.
func TestWaitingDetailEndsWithTheWait(t *testing.T) {
	t.Parallel()
	const throttled = "Workflow processing has been postponed because too many workflows are already running"
	w := nativeWorkflow()
	w.Status.Phase, w.Status.Message = wf.WorkflowPending, throttled
	first := project(t, argojob.Projection{Plan: queuedPlan(), Workflow: w})
	if first.Steps[1].Detail != throttled {
		t.Fatalf("throttled step detail %q", first.Steps[1].Detail)
	}
	w.Status.Phase, w.Status.Message = wf.WorkflowRunning, ""
	build := nativeNode("build", ".build", wf.NodeRunning, 1)
	w.Status.Nodes[build.ID] = build
	running := next(t, w, first)
	if s := running.Steps[1]; running.Status != stepmodel.RunRunning || s.DisplayReason != "waiting to start" || s.Detail != "" {
		t.Fatalf("started run keeps the throttle detail: %s %+v", running.Status, s)
	}
	build.Phase = wf.NodeFailed
	w.Status.Nodes[build.ID] = build
	w.Status.Phase = wf.WorkflowFailed
	finished := next(t, w, running)
	if s := finished.Steps[1]; s.Status != stepmodel.StepSkipped || s.Detail != "" {
		t.Fatalf("unreached step of a finished run keeps a detail: %+v", s)
	}
}

// A step whose retried template waited for a lock drops the lock-status detail
// once the lock is acquired, before any Pod exists.
func TestLockDetailEndsWhenTheLockIsAcquired(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	retry := wf.NodeStatus{ID: "r", Name: w.Name + ".test", Type: wf.NodeTypeRetry, Phase: wf.NodePending, SynchronizationStatus: &wf.NodeSynchronizationStatus{Waiting: "ci/ConfigMap/oberth-sync-limits/heavy-go"}}
	w.Status.Nodes[retry.ID] = retry
	first := project(t, argojob.Projection{Plan: queuedPlan(), Workflow: w})
	if first.Steps[1].Detail == "" {
		t.Fatal("lock wait has no detail")
	}
	retry.Phase, retry.SynchronizationStatus = wf.NodeRunning, nil
	w.Status.Nodes[retry.ID] = retry
	acquired := next(t, w, first)
	if s := acquired.Steps[1]; s.DisplayReason != "waiting to start" || s.Detail != "" {
		t.Fatalf("acquired lock keeps its detail: %+v", s)
	}
}

// A finished Workflow that still lists a node waiting for a lock gives the
// step it never reached no lock-status detail.
func TestFinishedWorkflowGivesNoWaitingDetail(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase, w.Spec.Shutdown = wf.WorkflowFailed, wf.ShutdownStrategyStop
	w.Status.Nodes["r"] = wf.NodeStatus{ID: "r", Name: w.Name + ".test", Type: wf.NodeTypeRetry, Phase: wf.NodePending, SynchronizationStatus: &wf.NodeSynchronizationStatus{Waiting: "ci/ConfigMap/oberth-sync-limits/heavy-go"}}
	out := project(t, argojob.Projection{Plan: queuedPlan(), Workflow: w})
	if s := out.Steps[1]; s.DisplayReason != "not reached" || s.Detail != "" {
		t.Fatalf("finished step keeps a waiting detail: %+v", s)
	}
}
