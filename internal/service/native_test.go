package service_test

import (
	"context"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestNativeProgressIsStoredWithoutEventOrAudit(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunQueued)
	if err := f.store.native.Progress(t.Context(), statefake.NativeProgress{ID: runID, Revision: 1, Status: stepmodel.RunRunning, Reason: "run is running", Detail: "full native detail"}); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Status(t.Context(), caller(), viewQuery{ID: runID})
	if err != nil || v.Detail != "full native detail" || v.DisplayReason == v.Detail {
		t.Fatalf("progress %+v %v", v, err)
	}
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{})
	if err != nil || len(p.Events) != 0 || len(f.store.native.Audits()) != 0 {
		t.Fatalf("progress event/audit %+v %v", p, err)
	}
}
func TestNativeFinishAtomicEventAndPublication(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	p, err := f.s.EventsSince(t.Context(), caller(), eventsInput{})
	if err != nil || len(p.Events) != 1 || p.Events[0].Kind != "run.finished" || len(f.store.native.PublicationValues()) != 1 || len(f.store.native.Audits()) != 1 {
		t.Fatalf("finish %+v %v", p, err)
	}
}
func TestNativeFailureReasonsAndEvidenceAreDetached(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	phase := stepmodel.NativeFailed
	failure := &stepmodel.FailureRecord{Class: stepmodel.FailureInfrastructure, Step: "build", Retry: stepmodel.RetryHint{Action: stepmodel.RetryRerun}}
	o := service.FinishedRun{RunID: runID, Status: stepmodel.RunFailed, Reason: "image pull failed", Detail: strings.Repeat("native detail ", 100), Failure: failure, NativePhase: &phase}
	if err := f.s.Finished(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	failure.Step = "changed"
	v, err := f.s.Status(t.Context(), caller(), viewQuery{ID: runID})
	if err != nil || v.DisplayReason != o.Reason || v.Detail != o.Detail || v.Failure == nil || v.Failure.Step != "build" || v.NativePhase == nil {
		t.Fatalf("failure %+v %v", v, err)
	}
}
func TestNativeProgressAfterTerminalRefusedByStorage(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	err := f.store.native.Progress(t.Context(), statefake.NativeProgress{ID: runID, Revision: 1, Status: stepmodel.RunRunning})
	requireCode(t, err, publicerr.Conflict)
	if f.store.Calls()["NativeProgress"] != 1 {
		t.Fatal("service duplicated terminal guard")
	}
}

func TestNativeReasonBoundAndInvalidVerdict(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	o := service.FinishedRun{RunID: runID, Status: stepmodel.RunFailed, Reason: strings.Repeat("x", 40)}
	if err := f.s.Finished(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	o.Reason += "x"
	requireField(t, f.s.Finished(t.Context(), o), "display_reason")
	o.Reason = "waiting..."
	requireField(t, f.s.Finished(t.Context(), o), "display_reason")
	o.Reason = ""
	o.Status = stepmodel.RunRunning
	requireField(t, f.s.Finished(t.Context(), o), "status")
}

func TestNativeInvalidPhaseAndFailure(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	phase := stepmodel.NativePhase("unknown")
	requireField(t, f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunFailed, NativePhase: &phase}), "native_phase")
	requireField(t, f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunFailed, Failure: &stepmodel.FailureRecord{Class: "invalid"}}), "failure")
}

type staleFinish struct {
	service.RunStore
	native *statefake.MovingRuns
}

func (s staleFinish) Finish(ctx context.Context, a stateport.Action, u stateport.RunUpdate) error {
	if _, err := s.native.Cancel(ctx, stateport.Action{Actor: "worker"}, stateport.RunCancellation{ID: u.ID, Revision: u.Revision}); err != nil {
		return err
	}
	return s.RunStore.Finish(ctx, a, u)
}
func TestFinishedUsesExactRevision(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	f.c.Runs = staleFinish{RunStore: f.store, native: f.store.native}
	f.restart(t)
	requireCode(t, f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunPassed}), publicerr.Conflict)
}
