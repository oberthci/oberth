package service_test

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// A run whose steps only wait reads queued with what it waits for in
// run_get, run_list, status and the web view (which read the same records),
// without a start; once a step starts it reads running, started, with the
// running reason, and a later wait does not queue it again.
func TestQueuedRunReadsQueuedWithItsWaitUntilAStepStarts(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunQueued)
	waited := at().Add(-time.Minute)
	steps := []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepQueued, StartedAt: &waited}, {Path: "test", Name: "test", Status: stepmodel.StepPending}}
	save := func(waiting string) {
		t.Helper()
		if err := f.store.native.SaveProgress(t.Context(), runID, stateport.Progress{Steps: steps, Waiting: waiting}); err != nil {
			t.Fatal(err)
		}
	}
	save("waiting for sync limit go")
	requireRunReads(t, f, runRead{stepmodel.RunQueued, "waiting for sync limit go", false})
	steps[0].Status = stepmodel.StepRunning
	save("")
	requireRunReads(t, f, runRead{stepmodel.RunRunning, "run is running", true})
	steps[0].Status = stepmodel.StepQueued
	save("waiting for a node")
	requireRunReads(t, f, runRead{stepmodel.RunRunning, "run is running", true})
}

type runRead struct {
	status  stepmodel.RunStatus
	reason  string
	started bool
}

func requireRunReads(t *testing.T, f *fixture, want runRead) {
	t.Helper()
	got, err := f.s.RunGet(t.Context(), caller(), viewQuery{ID: runID})
	if err != nil || (runRead{got.Status, got.DisplayReason, got.StartedAt != nil}) != want {
		t.Fatalf("run_get %s %q started %v, want %+v (%v)", got.Status, got.DisplayReason, got.StartedAt, want, err)
	}
	v, err := f.s.Status(t.Context(), caller(), viewQuery{ID: runID})
	if err != nil || v.Status != want.status || v.DisplayReason != want.reason {
		t.Fatalf("status %s %q (%v)", v.Status, v.DisplayReason, err)
	}
	requireRunListed(t, f, want)
}

func requireRunListed(t *testing.T, f *fixture, want runRead) {
	t.Helper()
	page, err := f.s.RunList(t.Context(), caller(), viewQuery{})
	if err != nil || len(page.Runs) != 1 || page.Runs[0].Status != want.status || page.Runs[0].DisplayReason != want.reason {
		t.Fatalf("run_list %+v (%v)", page.Runs, err)
	}
	filtered, err := f.s.RunList(t.Context(), caller(), viewQuery{Status: want.status})
	if err != nil || len(filtered.Runs) != 1 {
		t.Fatalf("run_list status=%s %+v (%v)", want.status, filtered.Runs, err)
	}
}
