package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

var (
	cancelAgent = api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent"}
	cancelAdmin = api.Actor{Identity: "dima@tuxbox", Fingerprint: "SHA256:dima", Admin: true}
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
	deadline := time.Now().Add(testTimeout(15 * time.Second))
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
	if run.Status != model.RunInterrupted || run.Phase != model.PhaseCancelled || run.Reason != want || run.SupersededBy != "" {
		t.Fatalf("run %s = status %s phase %q reason %q superseded_by %q; want cancelled with %q",
			run.ID, run.Status, run.Phase, run.Reason, run.SupersededBy, want)
	}
}

// A queued run is terminalized at once and never claimed.
func TestRunCancelQueuedRunIsNeverClaimed(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	queued := fixture.branch(t, fixture.beacon, "feature-q", "a")

	response := mustRunCancel(t, fixture.control, cancelAgent, map[string]any{"id": queued.ID[:8], "reason": "pushed by mistake"})
	if response.RunID != queued.ID || response.PreviousState != "queued" || response.Workflow != "none" ||
		response.Status != "interrupted" || response.Phase != "cancelled" || response.CancelledBy != cancelAgent.Identity ||
		response.Reason != "cancelled by agent@host: pushed by mistake" || response.Warning != "" {
		t.Fatalf("queued cancel response = %#v", response)
	}
	fixture.start(t)
	fixture.assertNothingCreated(t)
	assertCancelled(t, fixture.run(t, queued.ID), cancelAgent.Identity, "pushed by mistake")
	fixture.assertNoCIIssue(t, fixture.beacon, "feature-q")

	// run_get and run_list project who cancelled it and why.
	detail := callGroupRunGet(t, fixture.control, queued.ID)
	if detail.Run.Phase != model.PhaseCancelled || !strings.Contains(detail.Run.Reason, "agent@host: pushed by mistake") || detail.Scheduling != nil {
		t.Fatalf("run_get of a cancelled run = %#v", detail)
	}
	raw, _ := json.Marshal(map[string]any{"repo": "beacon", "ref": "feature-q"})
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

// Cancelling a waiting XL head releases its reservation at once: the next
// grant pass admits the runs it blocked, of its own group and of other
// repositories alike.
func TestRunCancelWaitingRunReleasesItsAdmissionReservation(t *testing.T) {
	fixture := newGroupFixture(t, 10)
	fixture.engine.declare("busy", "", periapsis.L)
	fixture.engine.declare("feat-kv", kubevirtGroup, periapsis.XL)
	fixture.engine.declare("feat-kv-2", kubevirtGroup, periapsis.S)
	fixture.engine.declare("small", "", periapsis.S)
	holder := fixture.branch(t, fixture.beacon, "busy", "a")
	head := fixture.branch(t, fixture.terraform, "feat-kv", "b")
	member := fixture.branch(t, fixture.terraform, "feat-kv-2", "c")
	small := fixture.branch(t, fixture.beacon, "small", "d")
	fixture.start(t)
	if created := fixture.awaitCreated(t, 1); !created[holder.ID] {
		t.Fatalf("created %v; want only the holder", created)
	}
	fixture.awaitAdmission(t, head, "waiting XL head", func(snapshot *model.AdmissionObservation) bool {
		return snapshot != nil && snapshot.State == "waiting" && snapshot.Position == 1
	})
	fixture.awaitAdmission(t, member, "waiting behind the head in its group", func(snapshot *model.AdmissionObservation) bool {
		return snapshot != nil && snapshot.GroupAhead == head.ID
	})

	// A non-admin may not cancel a concurrency-group member; nothing changes.
	if _, err := callRunCancel(t, fixture.control, cancelAgent, map[string]any{"id": head.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin cancel of a group member = %v", err)
	}
	fixture.assertNothingCreated(t)

	response := mustRunCancel(t, fixture.control, cancelAdmin, map[string]any{"id": head.ID, "reason": "wrong size"})
	if response.PreviousState != "waiting" || response.Workflow != "deleted" || response.Warning != "" {
		t.Fatalf("waiting cancel response = %#v", response)
	}
	if created := fixture.awaitCreated(t, 2); !created[member.ID] || !created[small.ID] {
		t.Fatalf("after the cancel the grant pass admitted %v; want %s and %s", created, member.ID, small.ID)
	}
	if snapshot := fixture.scheduler.AdmissionSnapshot(head.QueueSequence); snapshot != nil {
		t.Fatalf("cancelled head still holds a reservation: %#v", snapshot)
	}
	cancelled := fixture.awaitTerminal(t, head.ID)
	assertCancelled(t, cancelled, cancelAdmin.Identity, "wrong size")
	shown := fixture.control.presentationRun(t.Context(), cancelled)
	if shown.StartedAt != nil || shown.DisplayStatus != "cancelled" || shown.DisplayReason != "cancelled by "+cancelAdmin.Identity+": wrong size" {
		t.Fatalf("cancel during admission: %+v", shown)
	}
	fixture.awaitNoPendingCancellations(t)
	if fixture.engine.wasCreated(head.ID) {
		t.Fatal("a cancelled waiting run created its Workflow")
	}
	for _, run := range []model.Run{holder, member, small} {
		fixture.engine.finish(t, run.ID)
	}
	fixture.assertNothingCreated(t)
	fixture.assertNoCIIssue(t, fixture.terraform, "feat-kv")
	fixture.engine.assertNoViolations(t)
}

// A running run's Workflow is deleted; its owner settles without recording a
// result over the cancel, and its weight passes on only once the Workflow is
// gone.
func TestRunCancelRunningRunDeletesItsWorkflowAndKeepsTheCancel(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	fixture.engine.declare("busy", "", periapsis.M)
	fixture.engine.declare("heavy", "", periapsis.XL)
	running := fixture.branch(t, fixture.beacon, "busy", "a")
	next := fixture.branch(t, fixture.beacon, "heavy", "b")
	fixture.start(t)
	if created := fixture.awaitCreated(t, 1); !created[running.ID] {
		t.Fatalf("created %v; want the running run", created)
	}
	fixture.awaitAdmission(t, next, "XL waiting for the running run", func(snapshot *model.AdmissionObservation) bool {
		return snapshot != nil && snapshot.State == "waiting"
	})
	submitted := fixture.awaitRunStarted(t, running.ID)

	response := mustRunCancel(t, fixture.control, cancelAgent, map[string]any{"id": running.ID, "reason": "stuck test"})
	if response.PreviousState != "running" || response.Workflow != "deleted" {
		t.Fatalf("running cancel response = %#v", response)
	}
	if created := fixture.awaitCreated(t, 1); !created[next.ID] {
		t.Fatalf("after the deletion the gate admitted %v; want %s", created, next.ID)
	}
	fixture.awaitNoPendingCancellations(t)
	final := fixture.awaitTerminal(t, running.ID)
	assertCancelled(t, final, cancelAgent.Identity, "stuck test")
	if final.StartedAt == nil || !final.StartedAt.Equal(*submitted.StartedAt) {
		t.Fatalf("cancel rewrote the start of a running run: %#v", final)
	}
	fixture.engine.mu.Lock()
	deleted := slices.Contains(fixture.engine.deleted, submitted.JobName)
	fixture.engine.mu.Unlock()
	if !deleted {
		t.Fatalf("Workflow %s was not deleted", submitted.JobName)
	}
	// The owner's late result never replaces the cancel.
	time.Sleep(testTimeout(100 * time.Millisecond))
	assertCancelled(t, fixture.run(t, running.ID), cancelAgent.Identity, "stuck test")
	fixture.assertNoCIIssue(t, fixture.beacon, "busy")

	// Idempotence: a second cancel is a clear refusal and changes nothing.
	_, err := callRunCancel(t, fixture.control, cancelAgent, map[string]any{"id": running.ID})
	if !errors.Is(err, store.ErrRunNotActive) || !strings.Contains(err.Error(), "already cancelled") {
		t.Fatalf("second cancel = %v", err)
	}
	fixture.engine.finish(t, next.ID)
	failed := fixture.awaitTerminal(t, next.ID)
	if _, err := callRunCancel(t, fixture.control, cancelAgent, map[string]any{"id": failed.ID}); !errors.Is(err, store.ErrRunNotActive) ||
		!strings.Contains(err.Error(), "already failed") {
		t.Fatalf("cancel of a failed run = %v", err)
	}
	fixture.engine.assertNoViolations(t)
}

func (fixture *groupFixture) awaitRunStarted(t *testing.T, id string) model.Run {
	t.Helper()
	deadline := time.Now().Add(testTimeout(15 * time.Second))
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
	release := fixture.tag(t, fixture.beacon, "v1.0.0", "a", "b")
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
	grouped := branch
	grouped.ConcurrencyGroup = "terraform-state"
	groupedRunning := grouped
	groupedRunning.Status, groupedRunning.StartedAt = model.RunRunning, &started
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
		{"agent grouped", grouped, false, false, ErrForbidden, ""},
		{"agent tag", tag, false, true, ErrForbidden, ""},
		{"agent promotion", promotion, false, false, ErrForbidden, ""},
		{"agent schedule", schedule, false, false, ErrForbidden, ""},
		{"agent apply", apply, false, true, ErrForbidden, ""},
		{"admin branch", branch, true, false, nil, ""},
		{"admin grouped queued", grouped, true, false, nil, ""},
		{"admin grouped running", groupedRunning, true, false, nil, "manual recovery"},
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
	enqueued, promotion, err := fixture.store.EnqueuePromotionRun(t.Context(), model.RunSpec{
		RepoID: fixture.beacon.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbbbbbbbbbb",
		SHA: result, Actor: "agent@host", Trigger: "promotion", TestedSHA: result, BaseSHA: previous,
	}, model.PromotionSpec{
		RepoID: fixture.beacon.ID, SourceBranch: "feature/promote", SourceSHA: result,
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
	if response.PromotionID != promotion.ID || response.Workflow != "deleted" {
		t.Fatalf("promotion cancel response = %#v", response)
	}
	assertCancelled(t, fixture.awaitTerminal(t, enqueued.ID), cancelAdmin.Identity, "target frozen")
	fixture.awaitNoPendingCancellations(t)
	failed, err := fixture.store.Promotion(t.Context(), promotion.ID)
	if err != nil || failed.Status != model.PromotionFailed || !strings.Contains(failed.Error, "cancelled by dima@tuxbox: target frozen") {
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
	if issue, err := fixture.store.OpenCIIssue(t.Context(), fixture.beacon.ID, "feature/promote"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("promotion cancel opened issue %#v (%v)", issue, err)
	}
}

// A cancel that committed while the process died before deleting the
// Workflow is finished by the successor's startup cancellation pass, and the
// run is never re-fired, while an uncancelled stranded run of the same
// process is (proving the restart path ran).
func TestRunCancelIsNotRefiredAcrossARestart(t *testing.T) {
	fixture := newGroupFixture(t, 3)
	ctx := t.Context()
	cancelled := fixture.branch(t, fixture.beacon, "feature-cancel", "a")
	stranded := fixture.branch(t, fixture.beacon, "feature-stranded", "b")
	// A previous process claimed and submitted both, then died.
	for _, run := range []model.Run{cancelled, stranded} {
		if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.SetRunJobName(ctx, run.ID, "oberth-beacon-"+run.ID[:8]); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.MarkRunSubmitted(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.store.CancelRun(ctx, model.RunCancelRequest{RunID: cancelled.ID, Actor: cancelAgent.Identity, Reason: "abandon"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.RecoverOwnerState(ctx); err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(t.TempDir(), "logs"))
	if err != nil {
		t.Fatal(err)
	}
	successor, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: &controlGit{}, Logs: logs, Jobs: fixture.engine, ReleaseJobs: fixture.engine,
		Auditor: fixture.store, Signals: fixture.scheduler.signals, WorkspaceRoot: filepath.Join(t.TempDir(), "work"), MaxConcurrent: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(context.Background())
	var once sync.Once
	finished := make(chan error, 1)
	go func() { finished <- successor.Run(runCtx) }()
	shutdown := func() {
		once.Do(func() {
			stop()
			if err := <-finished; err != nil {
				t.Errorf("successor scheduler stopped with %v", err)
			}
		})
	}
	t.Cleanup(shutdown)

	// The stranded run is re-fired as a fresh queued copy that executes.
	created := fixture.awaitCreated(t, 1)
	requeued := fixture.run(t, stranded.ID)
	if requeued.Status != model.RunInterrupted || requeued.SupersededBy == "" || !created[requeued.SupersededBy] {
		t.Fatalf("stranded run after restart = %#v, created %v", requeued, created)
	}
	fixture.awaitNoPendingCancellations(t)
	fixture.assertNothingCreated(t)
	runs, err := fixture.store.ListRecentRuns(ctx, model.RunListFilter{RepoID: fixture.beacon.ID, Ref: "feature-cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("restart re-fired the cancelled run: %#v", runs)
	}
	assertCancelled(t, runs[0], cancelAgent.Identity, "abandon")
	fixture.engine.mu.Lock()
	deleted := slices.Contains(fixture.engine.deleted, cancelled.JobName) || slices.Contains(fixture.engine.deleted, "oberth-beacon-"+cancelled.ID[:8])
	fixture.engine.mu.Unlock()
	if !deleted {
		t.Fatal("the successor never deleted the cancelled run's Workflow")
	}
	fixture.assertNoCIIssue(t, fixture.beacon, "feature-cancel")
	fixture.engine.finish(t, requeued.SupersededBy)
	fixture.awaitTerminal(t, requeued.SupersededBy)
	shutdown()
}
