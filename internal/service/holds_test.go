package service_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func fixtureHold(id string) model.LiveHold {
	return model.LiveHold{HeldNotice: model.HeldNotice{RunID: id, Step: "build", NodeID: "node", PodUID: "pod-uid", HeldSince: at(), TTLSeconds: 1200}, PodName: "held-pod", RemainingMS: 1200000}
}
func heldFixture(t *testing.T) *fixture {
	t.Helper()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Origin.AssistRequested = true
	r.Origin.WorkflowName = "workflow"
	r.Origin.WorkflowUID = "workflow-uid"
	f.store.native.SeedRun(r)
	h := fixtureHold(r.ID)
	f.holds.setLive(r.ID, []model.LiveHold{h})
	if err := f.s.Held(t.Context(), h.HeldNotice); err != nil {
		t.Fatal(err)
	}
	return f
}
func TestHoldAuditPrecedesNativeEffects(t *testing.T) {
	t.Parallel()
	f := heldFixture(t)
	effects := 0
	failure := errors.New("inspection reply lost")
	f.holds.Execute = func(ctx context.Context, r stateport.Run, h model.LiveHold, q apicontract.RunHoldExecArgs) (apicontract.ExecResult, error) {
		effects++
		if q.Step != "build" || q.ID != r.ID {
			t.Fatal(q, r)
		}
		checkInspectionAudit(ctx, t, f, h)
		return apicontract.ExecResult{Stdout: "diagnostic output"}, failure
	}
	call, err := f.s.PrepareRunHoldExec(t.Context(), workerCaller(), apicontract.RunHoldExecArgs{ID: runID, Argv: []string{"diagnose", "build"}, Reason: "diagnose build"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(t.Context())
	if !errors.Is(err, failure) || out.Stdout != "diagnostic output" || effects != 1 {
		t.Fatal(out, err, effects)
	}
	r, _ := f.store.Run(t.Context(), runID)
	if r.Status != stepmodel.RunRunning || !r.Origin.AssistRequested || !r.Origin.Assisted {
		t.Fatal("inspection changed native result", r)
	}
	checkInspectedPassedRun(t, f)
}

func TestRefusedInspectionDoesNotMarkRun(t *testing.T) {
	t.Parallel()
	f := heldFixture(t)
	before, _ := f.store.Run(t.Context(), runID)
	audits := len(f.store.native.Audits())
	refusal := publicerr.New(publicerr.InvalidTransition, "secret step after inspection", "Push for a full run.", nil)
	f.holds.Preflight = func() error { return refusal }
	f.holds.Execute = func(context.Context, stateport.Run, model.LiveHold, apicontract.RunHoldExecArgs) (apicontract.ExecResult, error) {
		t.Fatal("refused inspection dispatched")
		return apicontract.ExecResult{}, nil
	}
	call, err := f.s.PrepareRunHoldExec(t.Context(), workerCaller(), apicontract.RunHoldExecArgs{ID: runID, Argv: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call(t.Context()); !errors.Is(err, refusal) {
		t.Fatal(err)
	}
	after, err := f.store.Run(t.Context(), runID)
	if err != nil || after.Origin.Assisted || after.Revision != before.Revision || len(f.store.native.Audits()) != audits {
		t.Fatal("refused inspection marked run", after, err)
	}
}
func TestHoldAuditFailureHasNoNativeEffect(t *testing.T) {
	t.Parallel()
	f := heldFixture(t)
	effects := 0
	f.holds.Execute = func(context.Context, stateport.Run, model.LiveHold, apicontract.RunHoldExecArgs) (apicontract.ExecResult, error) {
		effects++
		return apicontract.ExecResult{}, nil
	}
	call, err := f.s.PrepareRunHoldExec(t.Context(), workerCaller(), apicontract.RunHoldExecArgs{ID: runID, Argv: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("audit refused")
	f.store.WriteError = failure
	if _, err = call(t.Context()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if effects != 0 {
		t.Fatal("payload preceded audit", effects)
	}
}
func TestHoldReleaseAuditsExactIdentity(t *testing.T) {
	t.Parallel()
	f := heldFixture(t)
	called := false
	f.holds.Stop = func(_ context.Context, _ stateport.Run, h model.LiveHold) error {
		called = true
		a := f.store.native.Audits()
		last := a[len(a)-1]
		if h.PodUID != "pod-uid" || !strings.Contains(last.Details, "cannot repair") || !strings.Contains(last.Details, "pod-uid") {
			t.Fatal(last, h)
		}
		return nil
	}
	call, err := f.s.PrepareRunHoldAbandon(t.Context(), workerCaller(), apicontract.RunHoldAbandonArgs{ID: runID, Reason: "cannot repair"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call(t.Context()); err != nil || !called {
		t.Fatal(err, called)
	}
}
func TestHoldSelectionIsLiveAndUnambiguous(t *testing.T) {
	t.Parallel()
	f := heldFixture(t)
	second := fixtureHold(runID)
	second.Step = "test"
	second.NodeID = "second"
	second.PodUID = "second-uid"
	f.holds.setLive(runID, []model.LiveHold{fixtureHold(runID), second})
	call, err := f.s.PrepareRunHoldExec(t.Context(), workerCaller(), apicontract.RunHoldExecArgs{ID: runID, Argv: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = call(t.Context())
	requireField(t, err, "step")
	f.holds.setLive(runID, nil)
	_, err = call(t.Context())
	requireCode(t, err, publicerr.InvalidTransition)
}
func TestHoldTimingHintsDoNotAuthorizeOrExpire(t *testing.T) {
	t.Parallel()
	f := heldFixture(t)
	h := fixtureHold(runID)
	h.HeldSince = at().Add(-24 * time.Hour)
	h.RemainingMS = 0
	h.AvailabilityUnknown = true
	f.holds.setLive(runID, []model.LiveHold{h})
	f.offset.Store(int64(48 * time.Hour))
	called := false
	f.holds.Stop = func(context.Context, stateport.Run, model.LiveHold) error { called = true; return nil }
	call, err := f.s.PrepareRunHoldAbandon(t.Context(), workerCaller(), apicontract.RunHoldAbandonArgs{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call(t.Context()); err != nil || !called {
		t.Fatal("server clock guessed a hold outcome", err)
	}
}
func TestReleaseDoesNotWaitForInspection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := heldFixture(t)
		entered, done := make(chan struct{}), make(chan error, 1)
		unblock := make(chan struct{})
		stop := sync.OnceFunc(func() { close(unblock) })
		defer stop()
		f.holds.Execute = func(context.Context, stateport.Run, model.LiveHold, apicontract.RunHoldExecArgs) (apicontract.ExecResult, error) {
			close(entered)
			<-unblock
			return apicontract.ExecResult{}, nil
		}
		f.holds.Stop = func(context.Context, stateport.Run, model.LiveHold) error { stop(); return nil }
		inspect, err := f.s.PrepareRunHoldExec(t.Context(), workerCaller(), apicontract.RunHoldExecArgs{ID: runID, Argv: []string{"diagnose"}})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, err := inspect(t.Context()); done <- err }()
		<-entered
		release, err := f.s.PrepareRunHoldAbandon(t.Context(), workerCaller(), apicontract.RunHoldAbandonArgs{ID: runID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = release(t.Context()); err != nil {
			t.Fatal("release stayed behind inspection", err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func checkInspectionAudit(ctx context.Context, t *testing.T, f *fixture, h model.LiveHold) {
	t.Helper()
	current, _ := f.store.Run(ctx, h.RunID)
	audit := f.store.native.Audits()
	if len(audit) == 0 {
		t.Fatal("command dispatched before audit")
	}
	last := audit[len(audit)-1]
	if !current.Origin.AssistRequested || !current.Origin.Assisted || h.PodUID != "pod-uid" {
		t.Fatal(current, h)
	}
	if !strings.Contains(last.Details, `"Argc":2`) {
		t.Fatal("argument count not audited", last)
	}
	if !strings.Contains(last.Details, "diagnose build") || !strings.Contains(last.Details, "pod-uid") || !strings.Contains(last.Details, "ArgvSHA256") {
		t.Fatal("selected command audit missing", last)
	}
}

func checkInspectedPassedRun(t *testing.T, f *fixture) {
	t.Helper()
	phase := stepmodel.NativeSucceeded
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunPassed, NativePhase: &phase}); err != nil {
		t.Fatal(err)
	}
	r, err := f.store.Run(t.Context(), runID)
	if err != nil || r.Status != stepmodel.RunPassed || !r.Origin.Assisted {
		t.Fatal("tolerated failure lost native result or inspection mark", r, err)
	}
	if len(f.store.native.PublicationValues()) != 0 {
		t.Fatal("failed dispatch became publication evidence")
	}
	_, err = f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, err, publicerr.Forbidden)
}
