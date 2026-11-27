package service_test

import (
	"context"
	"errors"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type resolveHook struct {
	service.RunStore
	read func(context.Context, []string, string) (stateport.Run, error)
}

func (h resolveHook) ResolveRun(ctx context.Context, scope []string, id string) (stateport.Run, error) {
	return h.read(ctx, scope, id)
}

type waitOutcome struct {
	result apicontract.WaitResult
	err    error
}

// Covers: REQ-C2-005
func TestTruthfulConcurrentHistoryContract(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	read := make(chan struct{})
	release := make(chan struct{})
	f.c.Runs = resolveHook{RunStore: f.store, read: func(ctx context.Context, scope []string, id string) (stateport.Run, error) {
		r, err := f.store.ResolveRun(ctx, scope, id)
		close(read)
		<-release
		return r, err
	}}
	f.restart(t)
	before := f.store.Calls()
	done := make(chan waitOutcome, 1)
	go func() {
		r, err := f.s.RunWait(t.Context(), caller(), viewQuery{ID: runID}, time.Minute)
		done <- waitOutcome{r, err}
	}()
	receive(t, read)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunPassed, Detail: "native final detail"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	out := receive(t, done)
	if out.err != nil || out.result.Run == nil || out.result.Run.Status != stepmodel.RunPassed || out.result.Run.Detail != "native detail restricted" || out.result.TimedOut {
		t.Fatalf("wait %+v", out)
	}
	after := f.store.Calls()
	checkWaitCost(t, before, after)
}
func TestWaitUntilAnyAndHeld(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		for _, until := range []string{"any", "held"} {
			f := setup(t)
			r := f.seed(stepmodel.RunRunning)
			r.Origin.AssistRequested = true
			r.Origin.WorkflowUID = "workflow-uid"
			f.store.native.SeedRun(r)
			call, err := f.s.PrepareWait(t.Context(), caller(), viewQuery{ID: runID, Until: until}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if until == "held" {
				h := fixtureHold(runID)
				f.holds.setLive(runID, []model.LiveHold{h})
				err = f.s.Held(t.Context(), h.HeldNotice)
			} else {
				err = f.store.native.Progress(t.Context(), statefake.NativeProgress{ID: runID, Revision: 1, Status: stepmodel.RunRunning, Detail: "changed"})
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := call(t.Context())
			if err != nil || result.Run == nil || result.TimedOut {
				t.Fatalf("until %+v %v", result, err)
			}
			if until == "any" && result.Run.Detail != "native detail restricted" {
				t.Fatal("returned before a change")
			}
		}
	})
}
func TestWaitSelectsSHAAndRefAndID(t *testing.T) {
	t.Parallel()
	for _, q := range []viewQuery{{ID: runID[:8]}, {SHA: head[:8]}, {Ref: "refs/heads/topic"}} {
		f := setup(t)
		f.seed(stepmodel.RunPassed)
		result, err := f.s.Wait(t.Context(), caller(), q, time.Minute)
		if err != nil || result.Run == nil || result.Run.ID != runID {
			t.Fatalf("selected %+v %v", result, err)
		}
	}
}
func TestWaitTimeoutReturnsCurrentRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.seed(stepmodel.RunQueued)
		result, err := f.s.RunWait(t.Context(), caller(), viewQuery{ID: runID}, time.Nanosecond)
		if err != nil || !result.TimedOut || result.Run == nil || result.Run.Status != stepmodel.RunQueued {
			t.Fatalf("timeout %+v %v", result, err)
		}
	})
}
func TestWaitCanceledObservationDoesNotCancelRun(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	call, err := f.s.PrepareWait(t.Context(), caller(), viewQuery{ID: runID}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = call(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r, _ := f.store.Run(t.Context(), runID)
	if r.Status != stepmodel.RunRunning {
		t.Fatal("observation canceled run")
	}
}
func TestWaitMissingRunAndInvalidOptions(t *testing.T) {
	t.Parallel()
	f := setup(t)
	_, err := f.s.RunWait(t.Context(), caller(), viewQuery{ID: runID}, time.Minute)
	requireCode(t, err, publicerr.NotFound)
	_, err = f.s.PrepareWait(t.Context(), caller(), viewQuery{ID: runID, Until: "unknown"}, time.Minute)
	requireField(t, err, "until")
}
func TestWaitShutdownUnblocks(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	call, err := f.s.PrepareWait(t.Context(), caller(), viewQuery{ID: runID}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = call(t.Context())
	requireCode(t, err, publicerr.Unavailable)
}

func TestServiceRestartObservesSameDurableRunWithoutStarting(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	f.starter.Start = func(context.Context, service.StartRequest) (apicontract.Receipt, error) {
		t.Fatal("service started work during restart")
		return apicontract.Receipt{}, nil
	}
	if err := f.s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	out, err := f.s.RunWait(t.Context(), caller(), viewQuery{ID: r.ID}, time.Minute)
	if err != nil || out.Run == nil || out.Run.ID != r.ID || out.Run.Status != stepmodel.RunPassed {
		t.Fatalf("restart %+v %v", out, err)
	}
}

func checkWaitCost(t *testing.T, before, after map[string]int) {
	t.Helper()
	if after["ResolveRun"]-before["ResolveRun"] != 1 || after["Run"]-before["Run"] != 2 || after["RunState"]-before["RunState"] != 1 || after["Current"]-before["Current"] != 3 || after["Steps"] != before["Steps"] || after["Runs"] != before["Runs"] {
		t.Fatalf("wait calls %v -> %v (includes finish and fresh binding after its revision)", before, after)
	}
}
