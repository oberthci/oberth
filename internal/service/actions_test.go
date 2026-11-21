package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestCancelStopThenTerminateAuditReason(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	modes := []bool{}
	f.exec.CancelFunc = func(_ context.Context, _ stateport.Run, terminate bool) error {
		modes = append(modes, terminate)
		return nil
	}
	for range 3 {
		if _, err := f.s.Cancel(t.Context(), caller(), actionInput{ID: runID, Reason: "stop this run"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(modes) != 3 || modes[0] || !modes[1] || !modes[2] {
		t.Fatalf("modes %v", modes)
	}
	for _, a := range f.store.native.Audits() {
		if !strings.Contains(a.Details, "stop this run") {
			t.Fatal("lost audit reason", a)
		}
	}
}
func TestCancelScheduleStopsOnFirstRequest(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Origin.Trigger = model.TriggerSchedule
	f.store.native.SeedRun(r)
	f.exec.CancelFunc = func(_ context.Context, _ stateport.Run, terminate bool) error {
		if terminate {
			t.Fatal("schedule terminated on first cancel")
		}
		return nil
	}
	if _, err := f.s.Cancel(t.Context(), admin(), actionInput{ID: runID}); err != nil {
		t.Fatal(err)
	}
}
func TestTagAndPromotionTerminateAtOnce(t *testing.T) {
	t.Parallel()
	for _, trigger := range []model.RunTrigger{model.TriggerTag, model.TriggerPromotion} {
		t.Run(string(trigger), func(t *testing.T) {
			f := setup(t)
			r := f.seed(stepmodel.RunRunning)
			r.Origin.Trigger = trigger
			if trigger == model.TriggerTag {
				r.Ref = "refs/tags/v1.0.0"
			}
			f.store.native.SeedRun(r)
			f.exec.CancelFunc = func(_ context.Context, _ stateport.Run, terminate bool) error {
				if !terminate {
					t.Fatal("release-like run only stopped")
				}
				return nil
			}
			if _, err := f.s.Cancel(t.Context(), admin(), actionInput{ID: runID, Force: true}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestAgentCannotCancelOrRerunTag(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunFailed)
	r.Ref = "refs/tags/v1.0.0"
	r.Origin.Trigger = model.TriggerTag
	r.Release = true
	f.store.native.SeedRun(r)
	_, err := f.s.Cancel(t.Context(), caller(), actionInput{ID: runID, Force: true})
	requireCode(t, err, publicerr.Forbidden)
	_, err = f.s.Rerun(t.Context(), caller(), actionInput{ID: runID})
	requireCode(t, err, publicerr.Forbidden)
}
func TestTagCancelRequiresForce(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Ref = "refs/tags/v1.0.0"
	f.store.native.SeedRun(r)
	_, err := f.s.Cancel(t.Context(), admin(), actionInput{ID: runID})
	requireField(t, err, "force")
}
func TestCancelRecordsBeforeNativeEffectAndRereads(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	f.exec.CancelFunc = func(ctx context.Context, _ stateport.Run, _ bool) error {
		if len(f.store.native.Audits()) != 1 {
			t.Fatal("effect before audit")
		}
		return f.s.Finished(ctx, service.FinishedRun{RunID: runID, Status: stepmodel.RunCancelled})
	}
	out, err := f.s.Cancel(t.Context(), caller(), actionInput{ID: runID})
	if err != nil || out.State != string(stepmodel.RunCancelled) {
		t.Fatalf("cancel %+v %v", out, err)
	}
}
func TestCancelWriteFailurePreventsEffect(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	f.store.WriteError = errors.New("write unavailable")
	f.exec.CancelFunc = func(context.Context, stateport.Run, bool) error {
		t.Fatal("native effect after write failure")
		return nil
	}
	_, err := f.s.Cancel(t.Context(), caller(), actionInput{ID: runID})
	if !errors.Is(err, f.store.WriteError) {
		t.Fatal(err)
	}
}
func TestTerminalCancelRefusedByStore(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	f.exec.CancelFunc = func(context.Context, stateport.Run, bool) error { t.Fatal("terminal native effect"); return nil }
	_, err := f.s.Cancel(t.Context(), caller(), actionInput{ID: runID})
	requireCode(t, err, publicerr.Conflict)
	if len(f.store.native.Audits()) != 0 {
		t.Fatal("terminal cancel audited")
	}
}
func TestPreparedCancelCASDoesNotOverwriteNewObligation(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	first, err := f.s.PrepareCancel(t.Context(), caller(), actionInput{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Cancel(t.Context(), caller(), actionInput{ID: runID}); err != nil {
		t.Fatal(err)
	}
	_, err = first(t.Context())
	requireCode(t, err, publicerr.Conflict)
}
func TestRerunDelegatesExactSourceAssistAndReason(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunFailed)
	called := false
	f.starter.Start = func(_ context.Context, q service.StartRequest) (apicontract.Receipt, error) {
		called = true
		if q.Request.Repo != repository || q.Request.Actor.Identity != caller().Identity || q.Request.CommitSHA == nil || *q.Request.CommitSHA != head || !q.Request.Assist || q.Reason != "try again" || q.RerunOf != runID {
			t.Fatalf("starter %+v", q)
		}
		return apicontract.Receipt{ID: "new-run"}, nil
	}
	out, err := f.s.Rerun(t.Context(), caller(), actionInput{ID: runID, Assist: true, Reason: "try again"})
	if err != nil || !called || out.ID != "new-run" || f.store.Calls()["AcceptBatch"] != 0 {
		t.Fatalf("rerun %+v %v", out, err)
	}
}
func TestRerunRefusesMovedSourceAndIneligibleRuns(t *testing.T) {
	t.Parallel()
	for _, status := range []stepmodel.RunStatus{stepmodel.RunPassed, stepmodel.RunRunning, stepmodel.RunQueued} {
		f := setup(t)
		f.seed(status)
		_, err := f.s.Rerun(t.Context(), caller(), actionInput{ID: runID})
		requireCode(t, err, publicerr.InvalidTransition)
	}
	f := setup(t)
	f.seed(stepmodel.RunFailed)
	f.git.Resolve = func(_ context.Context, _ string, ref string) (gitport.Ref, error) {
		return gitport.Ref{Name: ref, ObjectSHA: strings.Repeat("b", 40)}, nil
	}
	_, err := f.s.Rerun(t.Context(), caller(), actionInput{ID: runID})
	requireCode(t, err, publicerr.Conflict)
}
func TestPurgeFailureKeepsPendingReceiptAndLogsReadable(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunFailed)
	failed := errors.New("log removal failed")
	f.logs.PurgeFunc = func(context.Context, stateport.Run) error { return failed }
	q := actionInput{ID: runID, What: "logs", Reason: "reclaim space"}
	_, err := f.s.Purge(t.Context(), admin(), q)
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	p, err := f.store.PurgeReceipt(t.Context(), runID)
	if err != nil || p.Completed {
		t.Fatal("failed removal marked purged", p, err)
	}
	logs, err := f.s.RunLogs(t.Context(), caller(), logRequest{viewQuery: viewQuery{ID: runID}})
	if err != nil || logs.Content == "" {
		t.Fatal("pending purge hid output", logs, err)
	}
	f.logs.PurgeFunc = func(context.Context, stateport.Run) error { return nil }
	if _, err := f.s.Purge(t.Context(), admin(), q); err != nil {
		t.Fatal(err)
	}
	logs, err = f.s.RunLogs(t.Context(), caller(), logRequest{viewQuery: viewQuery{ID: runID}})
	if err != nil || logs.DisplayReason != "logs purged" {
		t.Fatal("completed purge", logs, err)
	}
}
func TestPurgeProtectedTerminalRunRefused(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunRunning)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: runID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	f.logs.PurgeFunc = func(context.Context, stateport.Run) error { t.Fatal("protected bytes removed"); return nil }
	_, err := f.s.Purge(t.Context(), admin(), actionInput{ID: runID, What: "logs", Reason: "space"})
	requireCode(t, err, publicerr.Conflict)
}
func TestPurgeRequiresLogsAndReason(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunFailed)
	for _, q := range []actionInput{{ID: runID, What: "all", Reason: "space"}, {ID: runID, What: "logs"}} {
		_, err := f.s.Purge(t.Context(), admin(), q)
		requireCode(t, err, publicerr.InvalidInput)
	}
}
func TestCheckFindingsAndOverlaysReachWiring(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.exec.InspectFunc = func(_ context.Context, q service.CheckRequest) (apicontract.RunCheckResult, error) {
		if q.Request.Repo != repository || q.Request.Actor.Identity != caller().Identity || q.Request.Ref != "refs/heads/main" || len(q.Files) != 1 {
			t.Fatalf("inspection %+v", q)
		}
		return apicontract.RunCheckResult{Findings: []apicontract.Finding{{Code: "grant", Reason: "credential needs a grant"}}}, nil
	}
	out, err := f.s.Check(t.Context(), caller(), service.CheckRequest{Request: model.CIRequest{Repo: "widget"}, Files: []apicontract.RunCheckArgsFilesItem{{Path: ".oberth/build.yaml", Content: "spec: {}"}}})
	if err != nil || out.OK || len(out.Findings) != 1 || out.Checks == nil {
		t.Fatalf("check %+v %v", out, err)
	}
}
func TestHiddenActionTargetsRefuseBeforeNativeEffect(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunFailed)
	r.Repo = "private/team/widget"
	f.store.native.SeedRun(r)
	for _, a := range []actor.Actor{caller()} {
		_, err := f.s.Rerun(t.Context(), a, actionInput{ID: runID})
		requireCode(t, err, publicerr.NotFound)
	}
}

func TestScheduledRerunPreservesEntry(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunFailed)
	r.Origin.Trigger = model.TriggerSchedule
	r.Origin.ScheduleEntry = "nightly"
	f.store.native.SeedRun(r)
	called := false
	f.starter.Start = func(_ context.Context, q service.StartRequest) (apicontract.Receipt, error) {
		called = true
		if q.Request.Trigger != model.TriggerSchedule || q.Request.ScheduleEntry != "nightly" || q.RerunOf != r.ID {
			t.Fatal("scheduled rerun lost origin", q)
		}
		return apicontract.Receipt{ID: "rerun"}, nil
	}
	_, err := f.s.Rerun(t.Context(), admin(), actionInput{ID: r.ID})
	if err != nil || !called {
		t.Fatal("scheduled rerun refused", err)
	}
}
