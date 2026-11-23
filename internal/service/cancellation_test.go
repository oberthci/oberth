package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestQueuedCancelTerminatesAndNamesCaller(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunQueued)
	f.exec.CancelFunc = func(ctx context.Context, _ stateport.Run, terminate bool) error {
		if !terminate {
			t.Fatal("queued cancellation left exit handlers or lock waiting enabled")
		}
		return f.s.Finished(ctx, service.FinishedRun{RunID: runID, Status: stepmodel.RunCancelled, Reason: "stopped", Detail: "native shutdown"})
	}
	if _, err := f.s.Cancel(t.Context(), caller(), actionInput{ID: runID}); err != nil {
		t.Fatal(err)
	}
	call, err := f.s.PrepareRunGet(t.Context(), workerCaller(), apicontract.RunGetArgs{ID: runID})
	run, err := execute(t.Context(), call, err)
	if err != nil || run.Status != stepmodel.RunCancelled || run.DisplayReason != "cancelled by "+caller().Identity || !strings.Contains(run.Detail, "Cancelled by "+caller().Identity+".") {
		t.Fatal("cancellation lost its actor", run, err)
	}
}

func TestCancellationAttributionSurvivesRestartAndFitsReason(t *testing.T) {
	t.Parallel()
	for _, identity := range []string{"another-worker", strings.Repeat("long-worker-", 8)} {
		f := setup(t)
		run := f.seed(stepmodel.RunRunning)
		_, err := f.store.native.Cancel(t.Context(), stateport.Action{Actor: identity}, stateport.RunCancellation{ID: run.ID, Revision: run.Revision})
		if err != nil {
			t.Fatal(err)
		}
		f.restart(t)
		if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: run.ID, Status: stepmodel.RunCancelled, Reason: "stopped"}); err != nil {
			t.Fatal(err)
		}
		got, err := f.store.native.Run(t.Context(), run.ID)
		if err != nil || len(got.DisplayReason) > 40 || !strings.Contains(got.Detail, identity) || strings.Contains(got.Detail, "Cancelled by "+caller().Identity+".") {
			t.Fatal("cancel actor was lost or replaced by the push actor", got, err)
		}
	}
}

func TestCancelledRunExplainsHowToStartAgain(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunCancelled, Reason: "stopped"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.native.Run(t.Context(), runID)
	if err != nil || got.Failure != nil || !strings.Contains(got.Detail, "This run was cancelled. Push a new commit or request a rerun to run again.") || len(got.Detail) > 120 {
		t.Fatal("cancelled run offers code repair or lacks restart guidance", got, err)
	}
}
