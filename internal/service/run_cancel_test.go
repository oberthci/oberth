package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

var (
	cancelAgent = api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent"}
	cancelAdmin = api.Actor{Identity: "dima@example-node", Fingerprint: "SHA256:dima", Admin: true}
)

func callRunCancel(t *testing.T, control *API, actor api.Actor, arguments map[string]any) (api.RunCancelResponse, error) {
	t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	value, err := control.CallTool(t.Context(), actor, "run_cancel", raw)
	if err != nil {
		return api.RunCancelResponse{}, err
	}
	return value.(api.RunCancelResponse), nil
}

func mustRunCancel(t *testing.T, control *API, actor api.Actor, arguments map[string]any) api.RunCancelResponse {
	t.Helper()
	response, err := callRunCancel(t, control, actor, arguments)
	if err != nil {
		t.Fatalf("run_cancel %v: %v", arguments, err)
	}
	return response
}

func (fixture *groupFixture) assertNoCIIssue(t *testing.T, repository model.Repository, ref string) {
	t.Helper()
	if issue, err := fixture.store.OpenCIIssue(t.Context(), repository.ID, ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancelled run on %s opened CI issue %#v (%v)", ref, issue, err)
	}
}

func (fixture *groupFixture) awaitNoPendingCancellations(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(testStartTimeout(15 * time.Second))
	for {
		pending, err := fixture.store.PendingRunCancellations(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancellation obligations never settled: %#v", pending)
		}
		time.Sleep(testTimeout(5 * time.Millisecond))
	}
}

func assertCancelled(t *testing.T, run model.Run, actor, reason string) {
	t.Helper()
	want := "cancelled by " + actor
	if reason != "" {
		want += ": " + reason
	}
	if run.Status != model.RunInterrupted || run.Phase != model.PhaseCancelled || run.Reason != want {
		t.Fatalf("run %s = status %s phase %q reason %q; want cancelled with %q",
			run.ID, run.Status, run.Phase, run.Reason, want)
	}
}

// A queued run is terminalized at once and never claimed.
func TestRunCancelQueuedRunIsNeverClaimed(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	queued := fixture.branch(t, fixture.exampleService, "feature-q", "a")

	response := mustRunCancel(t, fixture.control, cancelAgent, map[string]any{"id": queued.ID[:8], "reason": "pushed by mistake"})
	if response.RunID != queued.ID || response.PreviousState != "queued" || response.Workflow != "none" ||
		response.Status != "interrupted" || response.Phase != "cancelled" || response.CancelledBy != cancelAgent.Identity ||
		response.Reason != "cancelled by agent@host: pushed by mistake" || response.Warning != "" {
		t.Fatalf("queued cancel response = %#v", response)
	}
	fixture.start(t)
	fixture.assertNothingCreated(t)
	assertCancelled(t, fixture.run(t, queued.ID), cancelAgent.Identity, "pushed by mistake")
	fixture.assertNoCIIssue(t, fixture.exampleService, "feature-q")

	// run_get and run_list project who cancelled it and why.
	detail := callGroupRunGet(t, fixture.control, queued.ID)
	if detail.Run.Phase != model.PhaseCancelled || !strings.Contains(detail.Run.Reason, "agent@host: pushed by mistake") || detail.Scheduling != nil {
		t.Fatalf("run_get of a cancelled run = %#v", detail)
	}
	raw, _ := json.Marshal(map[string]any{"repo": "example-service", "ref": "feature-q"})
	value, err := fixture.control.CallTool(t.Context(), cancelAgent, "run_list", raw)
	if err != nil {
		t.Fatal(err)
	}
	runs := value.(api.RunListResponse).Runs.([]model.Run)
	if len(runs) != 1 || !runs[0].Cancelled() || runs[0].Reason != detail.Run.Reason {
		t.Fatalf("run_list of a cancelled run = %#v", runs)
	}
	audit, err := fixture.store.LatestAuditActionByType(t.Context(), "run.cancel", "run")
	if err != nil || audit.Actor != cancelAgent.Identity || audit.ResourceID != queued.ID {
		t.Fatalf("run.cancel audit = %#v, %v", audit, err)
	}
}

// Cancelling preparation joins its creator without submitting a Workflow or
// blocking the independently claimed peer. Ownership lasts through worker join.
func TestRunCancelWaitingRunJoinsCreatorAndPreservesPeer(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	lift := fixture.pausePreparation(t)
	head := fixture.branch(t, fixture.terraform, "feat-kv", "b")
	member := fixture.branch(t, fixture.terraform, "feat-kv-2", "c")
	fixture.start(t)
	fixture.awaitPreparing(t, head)
	fixture.assertNothingCreated(t)

	response := mustRunCancel(t, fixture.control, cancelAdmin, map[string]any{"id": head.ID, "reason": "wrong size"})
	if response.PreviousState != "waiting" || !strings.Contains(response.Warning, "Terminate; exit handlers will be skipped") {
		t.Fatalf("waiting cancel response = %#v", response)
	}
	cancelled := fixture.awaitTerminal(t, head.ID)
	until := time.Now().Add(testStartTimeout(5 * time.Second))
	for fixture.scheduler.ownerForTest(head.QueueSequence) != nil {
		if time.Now().After(until) {
			t.Fatal("cancelled creator did not join and release ownership")
		}
		time.Sleep(testTimeout(time.Millisecond))
	}
	lift()
	assertCancelled(t, cancelled, cancelAdmin.Identity, "wrong size")
	shown := fixture.control.presentationRun(t.Context(), cancelled)
	if shown.StartedAt != nil || shown.DisplayStatus != "cancelled" || shown.DisplayReason != "by "+cancelAdmin.Identity+": wrong size" {
		t.Fatalf("cancel during admission: %+v", shown)
	}
	fixture.awaitNoPendingCancellations(t)
	if created := fixture.awaitCreated(t, 1); !created[member.ID] {
		t.Fatalf("cancelled waiter held the next run: %v", created)
	}
	if fixture.engine.wasCreated(head.ID) {
		t.Fatal("a cancelled waiting run created its Workflow")
	}
	fixture.engine.finish(t, member.ID)
	fixture.assertNothingCreated(t)
	fixture.assertNoCIIssue(t, fixture.terraform, "feat-kv")
	fixture.engine.assertNoViolations(t)
}

// Cancelling one submitted Workflow preserves the independently submitted peer
// and waits for the native verdict after exit handlers finish.
func TestRunCancelWaitsForNativeStopAndExitHandlers(t *testing.T) {
	fixture := newGroupFixture(t, 1)
	fixture.engine.holdStop = true
	running := fixture.branch(t, fixture.exampleService, "busy", "a")
	peer := fixture.branch(t, fixture.exampleService, "peer", "b")
	fixture.start(t)
	fixture.awaitCreated(t, 2)
	fixture.awaitRunStarted(t, running.ID)
	first := mustRunCancel(t, fixture.control, cancelAgent, map[string]any{"id": running.ID, "reason": "stuck test"})
	if first.Workflow != "pending" || first.Status != "running" {
		t.Fatalf("cancel = %+v", first)
	}
	current := fixture.run(t, running.ID)
	if current.Status != model.RunRunning || current.FinishedAt != nil || !strings.HasPrefix(current.Reason, "cancelling by ") {
		t.Fatalf("Stop acceptance terminalized onExit: %+v", current)
	}
	if fixture.run(t, peer.ID).Status.Terminal() {
		t.Fatal("Stop affected peer")
	}
	pending, err := fixture.store.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatalf("onExit lost custody: %+v %v", pending, err)
	}
	fixture.engine.finish(t, running.ID)
	final := fixture.awaitTerminal(t, running.ID)
	if final.Status != model.RunInterrupted || final.Phase != model.PhaseCancelled || !strings.Contains(final.Reason, "cancelled by agent@host: stuck test") {
		t.Fatalf("native Stop verdict = %+v", final)
	}
	fixture.awaitNoPendingCancellations(t)
	fixture.engine.finish(t, peer.ID)
}

func (fixture *groupFixture) awaitRunStarted(t *testing.T, id string) model.Run {
	t.Helper()
	deadline := time.Now().Add(testStartTimeout(15 * time.Second))
	for {
		run := fixture.run(t, id)
		if run.StartedAt != nil {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never started: %#v", id, run)
		}
		time.Sleep(testTimeout(5 * time.Millisecond))
	}
}

// Release runs are admin-only and need an explicit force; the cancel records
// the partial-publication warning in the response and the audit chain.
func TestRunCancelReleaseRunRequiresAdminAndForce(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	release := fixture.tag(t, fixture.exampleService, "v1.0.0", "a", "b")
	if _, err := callRunCancel(t, fixture.control, cancelAgent, map[string]any{"id": release.ID, "force": true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin release cancel = %v", err)
	}
	if _, err := callRunCancel(t, fixture.control, cancelAdmin, map[string]any{"id": release.ID}); !errors.Is(err, ErrInvalidInput) ||
		!strings.Contains(err.Error(), "force") {
		t.Fatalf("admin release cancel without force = %v", err)
	}
	if run := fixture.run(t, release.ID); run.Status != model.RunQueued {
		t.Fatalf("refused cancels changed the release run: %#v", run)
	}
	response := mustRunCancel(t, fixture.control, cancelAdmin, map[string]any{"id": release.ID, "force": true, "reason": "bad tag"})
	if !strings.Contains(response.Warning, "partial") {
		t.Fatalf("forced release cancel warning = %q", response.Warning)
	}
	audit, err := fixture.store.LatestAuditActionByType(t.Context(), "run.cancel", "run")
	if err != nil || !strings.Contains(audit.Details, `"force":true`) || !strings.Contains(audit.Details, "partial") {
		t.Fatalf("forced release cancel audit = %#v, %v", audit, err)
	}
	for _, bad := range []map[string]any{
		{"id": ""},
		{"id": release.ID, "reason": strings.Repeat("x", model.MaximumCancelReasonBytes+1)},
		{"id": release.ID, "reason": "line\nbreak"},
	} {
		if _, err := callRunCancel(t, fixture.control, cancelAdmin, bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("run_cancel %v = %v", bad, err)
		}
	}
}

func TestRunCancelPolicyMatrix(t *testing.T) {
	started := time.Now()
	branch := model.Run{ID: "b", RefKind: model.RefBranch, Trigger: "branch", Status: model.RunQueued}
	running := branch
	running.Status, running.StartedAt = model.RunRunning, &started
	tag := model.Run{ID: "t", RefKind: model.RefTag, Trigger: "tag", Release: true, Status: model.RunRunning}
	promotion := model.Run{ID: "p", RefKind: model.RefBranch, Trigger: "promotion", Status: model.RunRunning}
	schedule := model.Run{ID: "s", RefKind: model.RefBranch, Trigger: "schedule", Status: model.RunQueued}
	apply := model.Run{ID: "a", RefKind: model.RefBranch, Trigger: "apply", Status: model.RunQueued}
	for _, test := range []struct {
		name         string
		run          model.Run
		admin, force bool
		err          error
		warning      string
	}{
		{"agent branch", branch, false, false, nil, ""},
		{"agent running branch", running, false, false, nil, ""},
		{"agent tag", tag, false, true, ErrForbidden, ""},
		{"agent promotion", promotion, false, false, ErrForbidden, ""},
		{"agent schedule", schedule, false, false, ErrForbidden, ""},
		{"agent apply", apply, false, true, ErrForbidden, ""},
		{"admin branch", branch, true, false, nil, ""},
		{"admin running branch", running, true, false, nil, ""},
		{"admin promotion", promotion, true, false, nil, ""},
		{"admin tag", tag, true, false, ErrInvalidInput, ""},
		{"admin tag forced", tag, true, true, nil, "partial"},
		{"admin apply", apply, true, false, ErrInvalidInput, ""},
		{"admin apply forced", apply, true, true, nil, "partial"},
	} {
		warning, err := RunCancelPolicy(test.run, test.admin, test.force)
		if test.err == nil && err != nil || test.err != nil && !errors.Is(err, test.err) {
			t.Errorf("%s: err = %v, want %v", test.name, err, test.err)
		}
		if test.warning == "" && warning != "" || !strings.Contains(warning, test.warning) {
			t.Errorf("%s: warning = %q, want %q", test.name, warning, test.warning)
		}
	}
}

// Cancelling promotion CI fails the promotion cleanly: no publication begins
// and nothing is merged or pushed.
func TestRunCancelPromotionFailsThePromotionWithoutMerging(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	const (
		previous = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		result   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	enqueued, promotion, err := storetest.EnqueuePromotionRun(fixture.store, t.Context(), model.RunSpec{
		RepoID: fixture.exampleService.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbbbbbbbbbb",
		SHA: result, Actor: "agent@host", Trigger: "promotion", TestedSHA: result, BaseSHA: previous,
	}, model.PromotionSpec{
		RepoID: fixture.exampleService.ID, SourceBranch: "feature/promote", SourceSHA: result,
		TargetRef: "main", PreviousSHA: previous, ResultSHA: result, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.start(t)
	if created := fixture.awaitCreated(t, 1); !created[enqueued.ID] {
		t.Fatalf("created %v; want the promotion run", created)
	}
	if _, err := callRunCancel(t, fixture.control, cancelAgent, map[string]any{"id": enqueued.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin promotion cancel = %v", err)
	}
	response := mustRunCancel(t, fixture.control, cancelAdmin, map[string]any{"id": enqueued.ID, "reason": "target frozen"})
	if response.PromotionID != promotion.ID || response.Workflow != "pending" {
		t.Fatalf("promotion cancel response = %#v", response)
	}
	assertCancelled(t, fixture.awaitTerminal(t, enqueued.ID), cancelAdmin.Identity, "target frozen")
	fixture.awaitNoPendingCancellations(t)
	failed, err := fixture.store.Promotion(t.Context(), promotion.ID)
	if err != nil || failed.Status != model.PromotionFailed || !strings.Contains(failed.Error, "cancelled by dima@example-node: target frozen") {
		t.Fatalf("promotion after cancel = %#v, %v", failed, err)
	}
	if pending, err := fixture.store.PendingPublications(t.Context()); err != nil || len(pending) != 0 {
		t.Fatalf("pending publications after promotion cancel = %#v, %v", pending, err)
	}
	fixture.git.mu.Lock()
	pushed := append(append([]string(nil), fixture.git.promotions...), fixture.git.syncedBranches...)
	fixture.git.mu.Unlock()
	if len(pushed) != 0 {
		t.Fatalf("promotion cancel pushed %v", pushed)
	}
	if issue, err := fixture.store.OpenCIIssue(t.Context(), fixture.exampleService.ID, "feature/promote"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("promotion cancel opened issue %#v (%v)", issue, err)
	}
}

func TestRunReasonCannotRequestCancellation(t *testing.T) {
	for _, reason := range []string{"cancelling by owner", "stopped by owner", "ref deleted"} {
		t.Run(reason, func(t *testing.T) {
			f := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "test", Error: "native Workflow failure"})
			run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "reason", SHA: strings.Repeat("a", 40), Actor: "agent@host", Trigger: "branch", Reason: reason})
			if err != nil {
				t.Fatal(err)
			}
			if err = f.scheduler.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			final, err := f.store.Run(t.Context(), run.ID)
			if err != nil || final.Status != model.RunFailed || final.Phase != "test" || len(f.jobs.createdCI) != 1 {
				t.Fatalf("reason text controlled execution: %+v %v", final, err)
			}
		})
	}
}
