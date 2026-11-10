package argojob

import (
	"context"
	"slices"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

// postponedWorkflow is a Workflow exactly as the Argo v4.0.8 controller
// leaves it when its throttler does not admit it: Pending, no startedAt and
// the fixed postponement message, with no condition.
func postponedWorkflow(now time.Time) *wfv1.Workflow {
	wf := schedulingWorkflow()
	wf.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
	wf.Status.Phase, wf.Status.Message, wf.Status.StartedAt = wfv1.WorkflowPending, argoParallelismPostponed, metav1.Time{}
	return wf
}

// A Workflow Argo postpones for its parallelism limit is queued by Argo, not
// stalled: neither the startup bound nor the pre-start watchdog counts the
// wait, exactly as for a workflow-level lock (#1517). Once Argo admits it the
// message is replaced and the startup bound applies again.
func TestAdmissionDeadlineLeavesParallelismPostponementToArgo(t *testing.T) {
	now := time.Now()
	wf := postponedWorkflow(now)
	if !postponedByParallelism(wf) {
		t.Fatal("controller postponement not recognized")
	}
	watch := admissionProgress{}
	for _, at := range []time.Time{now, now.Add(2 * time.Hour)} {
		if err := watch.check(wf, nil, nil, at, 10*time.Minute); err != nil {
			t.Fatal("parallelism postponement failed the workflow", err)
		}
	}
	if origin, _ := watchdogOrigin(wf); !origin.IsZero() {
		t.Fatalf("pre-start watchdog counts a postponement from %v", origin)
	}
	if since := watchdogOriginSince(wf, now.Add(-time.Hour)); time.Since(since) > time.Minute {
		t.Fatalf("live watchdog not re-armed while postponed: %v", since)
	}
	wf.Status.Message = ""
	if err := watch.check(wf, nil, nil, now.Add(2*time.Hour+10*time.Minute), 10*time.Minute); err == nil {
		t.Fatal("startup stall after admission escaped the deadline")
	}
}

// Only the exact controller text, on an unstarted Pending Workflow, is a
// postponement; a started Workflow or a different message is not.
func TestParallelismPostponementIsTheExactControllerState(t *testing.T) {
	now := time.Now()
	for name, change := range map[string]func(*wfv1.Workflow){
		"running":       func(wf *wfv1.Workflow) { wf.Status.Phase = wfv1.WorkflowRunning },
		"started":       func(wf *wfv1.Workflow) { wf.Status.StartedAt = metav1.NewTime(now) },
		"other message": func(wf *wfv1.Workflow) { wf.Status.Message = argoParallelismPostponed + " (edited)" },
	} {
		wf := postponedWorkflow(now)
		change(wf)
		if postponedByParallelism(wf) {
			t.Fatalf("%s: recognized as postponed", name)
		}
	}
}

// The scheduling observation reports the postponement in fixed words, and
// the synchronization observation reports it as a wait on the parallelism
// key; an admitted Workflow holds that key.
func TestPostponementObservations(t *testing.T) {
	wf := postponedWorkflow(time.Now())
	controller := &Controller{workflows: &schedulingWorkflowClient{workflow: wf}, config: Config{Namespace: "pipeline"}}
	observation, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
	if err != nil || observation.Message != model.ParallelismWaitMessage || observation.Phase != "Pending" {
		t.Fatalf("scheduling observation = %#v, %v", observation, err)
	}
	sync, err := controller.ObserveSynchronization(context.Background(), "wf", "run")
	if err != nil || !slices.Equal(sync.Waiting, []string{model.SyncParallelismLock}) || len(sync.Holding) != 0 {
		t.Fatalf("postponed sync observation = %+v, %v", sync, err)
	}
	wf.Status.Phase, wf.Status.Message = wfv1.WorkflowPending, "Waiting for pipeline/Mutex/oberth-release lock. Lock status: 0/1"
	if sync, err = controller.ObserveSynchronization(context.Background(), "wf", "run"); err != nil || !slices.Equal(sync.Holding, []string{model.SyncParallelismLock}) {
		t.Fatalf("admitted lock-waiting sync observation = %+v, %v", sync, err)
	}
	wf.Status.Phase, wf.Status.Message = "", ""
	if sync, err = controller.ObserveSynchronization(context.Background(), "wf", "run"); err != nil || len(sync.Holding)+len(sync.Waiting) != 0 {
		t.Fatalf("unprocessed sync observation = %+v, %v", sync, err)
	}
}
