package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// This fake is Argo's global cap: accepting a Workflow never waits, while only
// two of the submitted Workflows are reported as running.
type cappedWorkflowEngine struct {
	*groupEngine
	parallelism int
}

func (e *cappedWorkflowEngine) ObserveScheduling(_ context.Context, run model.Run) (*model.ExecutionObservation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.byRun[run.ID] == nil {
		return nil, fmt.Errorf("Workflow was not submitted")
	}
	// Stable order for this controlled Argo observation; cap enforcement belongs to the engine.
	running := 0
	for _, job := range e.jobs {
		if job.runID < run.ID {
			running++
		}
	}
	if running < e.parallelism {
		return &model.ExecutionObservation{State: "observed", Phase: "Running", Message: "Workflow execution is running."}, nil
	}
	return &model.ExecutionObservation{State: "observed", Phase: "Pending", Message: model.ParallelismWaitMessage}, nil
}
func TestNativeArgoCapDoesNotHoldBackAnyOfTenSubmissions(t *testing.T) {
	f := newGroupFixture(t, 0)
	engine := &cappedWorkflowEngine{groupEngine: f.engine, parallelism: 2}
	f.scheduler.jobs = engine
	f.control.schedulingObserver = engine
	runs := make([]model.Run, 10)
	for i := range runs {
		runs[i] = f.branch(t, f.terraform, fmt.Sprintf("feature/native-%d", i), "a")
	}
	f.start(t)
	created := f.awaitCreated(t, 10)
	counts := map[string]int{}
	for _, run := range runs {
		if !created[run.ID] {
			t.Fatalf("run withheld: %s", run.ID)
		}
		current := f.awaitRunStarted(t, run.ID)
		status, err := f.control.statusForRun(t.Context(), f.terraform, current)
		if err != nil {
			t.Fatal(err)
		}
		counts[status.DisplayStatus]++
		if status.DisplayStatus == model.DisplayQueued && status.DisplayReason == "" {
			t.Fatal("native queue reason missing")
		}
	}
	if counts[model.DisplayRunning] != 2 || counts[model.DisplayQueued] != 8 {
		t.Fatal(counts)
	}
	for _, run := range runs {
		f.engine.finish(t, run.ID)
		f.awaitTerminal(t, run.ID)
	}
}
