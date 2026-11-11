package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Prepare under the gate lock so tests control every admission/release without
// sleeps, blocked background task goroutines or access to live reservation fields.
func priorityWaiter(g *admissionGate, seq int64, p model.RunPriority, at time.Time, _ pipeline.Size, _ ...time.Duration) *admissionReservation {
	r := g.reserveRun(model.Run{ID: string(rune('a' + seq)), QueueSequence: seq, AdmissionSequence: seq, Priority: p, QueuedAt: at})
	g.mu.Lock()
	r.prepared = true
	r.preparedAt = g.clock()
	g.grantLocked()
	g.mu.Unlock()
	return r
}

func TestPriorityAdmissionOrdersClassesThenFIFO(t *testing.T) {
	g := newAdmissionGate()
	holder := g.reserve(1)
	release, err := holder.acquire(context.Background(), pipeline.XL)
	if err != nil {
		t.Fatal(err)
	}
	blockOnDiskFloor(g)
	now := time.Now()
	normal := priorityWaiter(g, 2, model.PriorityStandard, now, pipeline.XL)
	high := priorityWaiter(g, 3, model.PriorityPromotion, now, pipeline.XL)
	highest := priorityWaiter(g, 4, model.PriorityHigh, now, pipeline.XL)
	highest2 := priorityWaiter(g, 5, model.PriorityHigh, now, pipeline.XL)
	release()
	assertWaiterOrder(t, g, highest, highest2, high, normal)
	clearDiskFloor(g)
	for _, r := range []*admissionReservation{highest, highest2, high, normal} {
		if !granted(g, r) {
			t.Fatal("waiter not admitted after the floor cleared")
		}
		r.release()
	}
}

func TestPriorityAdmissionStarvationReordersExistingWaiters(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	g := newAdmissionGate()
	g.now = clock.Now
	holder := g.reserve(1)
	release, err := holder.acquire(context.Background(), pipeline.XL)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	blockOnDiskFloor(g)
	normal := priorityWaiter(g, 2, model.PriorityStandard, clock.now.Add(-time.Hour+time.Nanosecond), pipeline.XL)
	highest := priorityWaiter(g, 3, model.PriorityHigh, clock.now, pipeline.XL)
	assertWaiterOrder(t, g, highest, normal)
	clock.now = clock.now.Add(time.Nanosecond)
	// Aging lifts normal, but never above base highest.
	assertWaiterOrder(t, g, highest, normal)
	clearDiskFloor(g)
	if !granted(g, highest) || !granted(g, normal) {
		t.Fatal("waiters not admitted after the floor cleared")
	}
	highest.release()
	normal.release()
}

func TestQueuePriorityToolsAdminOnlyAuditedAndGated(t *testing.T) {
	f := newControlFixture(t)
	ctx := context.Background()
	control, err := NewAPI(APIConfig{Runs: f.store, QueuePriorities: f.store, Enqueues: f.scheduler})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Shutdown(ctx) }()
	run, err := storetest.EnqueueRun(f.store, ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "rc/v1", SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	admin := api.Actor{Identity: "admin@host", Admin: true}
	for _, actor := range []api.Actor{{Identity: "dev"}, {Identity: "scoped-admin", Admin: true, Scope: []string{"codeberg"}}} {
		for tool, raw := range map[string]string{"queue_priority_config": `{"rc_patterns":["rc/*"]}`, "queue_priority_get": `{}`, "run_priority": `{"id":"` + run.ID + `","priority":"high","reason":"release blocker"}`} {
			if _, err = control.CallTool(ctx, actor, tool, json.RawMessage(raw)); !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s by %+v = %v", tool, actor, err)
			}
		}
	}
	got, err := f.store.Run(ctx, run.ID)
	if err != nil || got.Priority != model.PriorityStandard {
		t.Fatalf("nonadmin mutated priority: %+v %v", got, err)
	}
	if _, err = control.CallTool(ctx, admin, "queue_priority_config", json.RawMessage(`{"rc_patterns":["rc/*"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = control.CallTool(ctx, admin, "queue_priority_config", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing patterns=%v", err)
	}
	value, err := control.CallTool(ctx, admin, "run_priority", json.RawMessage(`{"id":"`+run.ID+`","priority":"high","reason":"release blocker"}`))
	if err != nil {
		t.Fatal(err)
	}
	shown := value.(model.Run)
	if shown.Priority != model.PriorityHigh || shown.DisplayReason != "in line" || !strings.Contains(shown.PriorityReason, "release blocker") {
		t.Fatalf("bump display=%+v", shown)
	}
	denied := errors.New("audit gate refused")
	control.mutationGate = func(context.Context) error { return denied }
	if _, err = control.CallTool(ctx, admin, "queue_priority_config", json.RawMessage(`{"rc_patterns":[]}`)); !errors.Is(err, denied) {
		t.Fatalf("config bypassed audit gate: %v", err)
	}
	if _, err = control.CallTool(ctx, admin, "run_priority", json.RawMessage(`{}`)); !errors.Is(err, denied) {
		t.Fatalf("bump bypassed audit gate: %v", err)
	}
	if !MutatingTool("run_priority") || !MutatingTool("queue_priority_config") || MutatingTool("queue_priority_get") {
		t.Fatal("priority tool mutation classification")
	}
}

func TestPriorityDispatchClaimsPastLowerClassWaitersAndRefreshesBumps(t *testing.T) {
	f := newControlFixture(t)
	ctx := context.Background()
	g := newAdmissionGate()
	scheduler := &Scheduler{store: f.store, admission: g}
	holder := g.reserve(-1)
	release, err := holder.acquire(ctx, pipeline.XL)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// The disk floor keeps the claimed runs waiting.
	blockOnDiskFloor(g)
	enqueue := func(ref string, kind model.RefKind) model.Run {
		t.Helper()
		trigger := "push"
		if kind == model.RefTag {
			trigger = "tag"
		}
		r, err := storetest.EnqueueRun(f.store, ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: kind, Ref: ref, SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: trigger})
		if err != nil {
			t.Fatal(err)
		}
		return r.Run
	}
	first := enqueue("feature/a", model.RefBranch)
	second := enqueue("feature/b", model.RefBranch)
	for _, want := range []model.Run{first, second} {
		r, err := f.store.ClaimNextRun(ctx)
		if err != nil || r.ID != want.ID {
			t.Fatal(err)
		}
		reservation := g.reserveRun(r)
		g.mu.Lock()
		reservation.prepared = true
		g.mu.Unlock()
	}
	if _, err = f.store.BumpRunPriority(ctx, "admin", second.ID, model.PriorityPromotion, "RC blocker"); err != nil {
		t.Fatal(err)
	}
	if err = scheduler.refreshPriorities(ctx); err != nil {
		t.Fatal(err)
	}
	if g.waiters[0].runID != second.ID {
		t.Fatal("admin bump did not reorder an existing reservation")
	}
	tag := enqueue("v1", model.RefTag)
	if _, err := f.store.BumpRunPriority(ctx, "admin", tag.ID, model.PriorityHigh, "urgent tag"); err != nil {
		t.Fatal(err)
	}
	if scheduler.dispatchSlotAvailable(2) {
		t.Fatal("ordinary dispatch claimed past a waiting run")
	}
	// Without a run ceiling the priority lookahead allows one extra claim
	// per class above low: three claims in flight.
	if available, err := scheduler.priorityDispatchAvailable(ctx, 2); err != nil || !available {
		t.Fatalf("release hidden behind dispatch ceiling: %v %v", available, err)
	}
	r, err := f.store.ClaimNextRun(ctx)
	if err != nil || r.ID != tag.ID {
		t.Fatal(err)
	}
	tagReservation := g.reserveRun(r)
	// The next release cannot pile up another same-class claim.
	enqueue("v2", model.RefTag)
	if available, err := scheduler.priorityDispatchAvailable(ctx, 3); err != nil || available {
		t.Fatalf("unbounded priority claims: %v %v", available, err)
	}
	g.mu.Lock()
	tagReservation.prepared = true
	g.mu.Unlock()
	release()
	assertWaiterOrder(t, g, tagReservation, g.reservations[second.QueueSequence])
	clearDiskFloor(g)
	if !granted(g, tagReservation) || !granted(g, g.reservations[second.QueueSequence]) {
		t.Fatal("waiters not admitted after the floor cleared")
	}
	tagReservation.release()
	for _, r := range g.reservations {
		r.cancel()
	}
}

func TestPriorityAdmissionAgesHighBeforeNewerNormal(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	g := newAdmissionGate()
	g.now = clock.Now
	holder := g.reserve(1)
	release, err := holder.acquire(t.Context(), pipeline.XL)
	if err != nil {
		t.Fatal(err)
	}
	blockOnDiskFloor(g)
	high := priorityWaiter(g, 2, model.PriorityHigh, clock.now, pipeline.XL)
	clock.now = clock.now.Add(time.Minute)
	normal := priorityWaiter(g, 3, model.PriorityStandard, clock.now, pipeline.XL)
	clock.now = clock.now.Add(time.Hour)
	release()
	assertWaiterOrder(t, g, high, normal)
	clearDiskFloor(g)
	if !granted(g, high) || !granted(g, normal) {
		t.Fatal("waiters not admitted after the floor cleared")
	}
	high.release()
	normal.release()
}

func TestQueuePriorityToolRefusesGrantedBeforeStartIsPersisted(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	control, err := NewAPI(APIConfig{Runs: f.store, QueuePriorities: f.store, Enqueues: f.scheduler})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Shutdown(ctx) }()
	enqueued, err := storetest.EnqueueRun(f.store, ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(ctx)
	if err != nil || run.ID != enqueued.ID {
		t.Fatalf("claim=%+v %v", run, err)
	}
	r := f.scheduler.admission.reserveRun(run)
	release, err := r.acquire(ctx, pipeline.M)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if run.StartedAt != nil {
		t.Fatal("test needs a grant without durable started_at")
	}
	_, err = control.CallTool(ctx, api.Actor{Identity: "admin", Admin: true}, "run_priority", json.RawMessage(`{"id":"`+run.ID+`","priority":"high","reason":"too late"}`))
	if !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("granted run bump=%v", err)
	}
	actions, _, err := f.store.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "run.priority"})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Action == "run.priority" {
			t.Fatalf("refused bump was audited as success: %+v", action)
		}
	}
}

func TestPriorityPresentationUsesActivePriorityAndKeepsTerminalCause(t *testing.T) {
	now := time.Now()
	run := model.Run{Status: model.RunQueued, Priority: model.PriorityHigh, PriorityReason: "RC", QueuedAt: now}
	if shown := presentationRunAt(run, now); shown.DisplayReason != "in line" || shown.PriorityReason != "RC" {
		t.Fatalf("active display=%+v", shown)
	}
	run.Status, run.Phase, run.Reason = model.RunInterrupted, model.PhaseCancelled, "cancelled by admin: obsolete"
	if shown := presentationRunAt(run, now); shown.DisplayReason != strings.TrimPrefix(run.Reason, "cancelled ") || shown.PriorityReason != "RC" {
		t.Fatalf("terminal display=%+v", shown)
	}
}

type prioritySQLiteError int

func (e prioritySQLiteError) Error() string { return fmt.Sprintf("SQLite lock %d", e) }
func (e prioritySQLiteError) Code() int     { return int(e) }

type busyPriorityStore struct {
	*store.Store
	refreshBusy atomic.Bool
	peekBusy    atomic.Bool
	observed    chan string
}

func (s *busyPriorityStore) RunsByIDs(ctx context.Context, ids []string) (map[string]model.Run, error) {
	if s.refreshBusy.CompareAndSwap(true, false) {
		s.observed <- "refresh"
		return nil, fmt.Errorf("refresh priorities: %w", prioritySQLiteError(261)) // SQLITE_BUSY_RECOVERY
	}
	return s.Store.RunsByIDs(ctx, ids)
}

func (s *busyPriorityStore) NextQueuedRun(ctx context.Context) (model.Run, error) {
	if s.peekBusy.CompareAndSwap(true, false) {
		s.observed <- "dispatch"
		return model.Run{}, fmt.Errorf("peek priorities: %w", prioritySQLiteError(5)) // SQLITE_BUSY
	}
	return s.Store.NextQueuedRun(ctx)
}

func TestPrioritySchedulerSurvivesBusyReadsAndResumesDispatch(t *testing.T) {
	for _, operation := range []string{"refresh", "dispatch"} {
		t.Run(operation, func(t *testing.T) {
			f := newGroupFixture(t, 2)
			busy := &busyPriorityStore{Store: f.store, observed: make(chan string, 1)}
			f.scheduler.store = busy
			f.engine.declare("running", "", pipeline.L)
			f.engine.declare("waiting", "", pipeline.L)
			f.engine.declare("rc/v1", "", pipeline.S)
			f.engine.declareCPU("running", 5900)
			f.engine.declareCPU("waiting", 5900)
			f.engine.declareCPU("rc/v1", 1900)
			if _, err := f.store.SetQueuePriorityPatterns(t.Context(), "admin", []string{"rc/*"}); err != nil {
				t.Fatal(err)
			}
			running := f.branch(t, f.terraform, "running", "a")
			f.start(t)
			f.awaitCreated(t, 1)
			// Host pressure keeps the ordinary run waiting in the gate, so the
			// priority refresh and lookahead read the store.
			lift := f.pressureValve(t)
			waiting := f.branch(t, f.terraform, "waiting", "b")
			f.awaitAdmission(t, waiting, "pressure-held run", func(snapshot *model.AdmissionObservation) bool {
				return snapshot != nil && snapshot.State == "waiting"
			})
			if operation == "refresh" {
				busy.refreshBusy.Store(true)
			} else {
				busy.peekBusy.Store(true)
			}
			rc := f.branch(t, f.terraform, "rc/v1", "c")
			f.scheduler.NotifyQueue()
			select {
			case got := <-busy.observed:
				if got != operation {
					t.Fatalf("busy read=%s", got)
				}
			case <-time.After(testStartTimeout(10 * time.Second)):
				t.Fatal("busy read was not exercised")
			}
			lift()
			if created := f.awaitCreated(t, 2); !created[rc.ID] || !created[waiting.ID] {
				t.Fatalf("dispatch did not recover after busy %s: %v", operation, created)
			}
			f.engine.finish(t, rc.ID)
			f.engine.finish(t, running.ID)
			f.engine.finish(t, waiting.ID)
			f.awaitTerminal(t, waiting.ID)
		})
	}
}

func TestPriorityReleaseAdmissionWithoutGrantAndBranchTransportSignals(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	if _, err := f.store.SizeGrant(ctx, f.repo.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fixture unexpectedly has a size grant: %v", err)
	}
	for _, trigger := range []string{"branch", "tag", "release"} {
		branch, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "priority-branch-" + trigger, Repository: f.repo, Branch: "release/" + trigger, SHA: strings.Repeat("a", 40), Actor: "pusher", Trigger: trigger})
		if err != nil || branch.Priority != model.PriorityStandard || branch.RefKind != model.RefBranch {
			t.Fatalf("branch transport claimed release urgency: %+v %v", branch, err)
		}
	}
	release, err := f.scheduler.AdmitRelease(ctx, ReleaseRequest{EventID: "priority-tag", Repository: f.repo, Tag: "v1.2.3", ObjectSHA: strings.Repeat("b", 40), CommitSHA: strings.Repeat("c", 40), Actor: "pusher"})
	if err != nil || release.Priority != model.PriorityPromotion || release.RefKind != model.RefTag || !release.Release {
		t.Fatalf("genuine ungranted tag failed priority admission: %+v %v", release, err)
	}
	next, err := f.store.NextQueuedRun(ctx)
	if err != nil || next.ID != release.ID {
		t.Fatalf("release remained behind ordinary branch queue: %+v %v", next, err)
	}
}

func TestPriorityWithheldTagPreservesAdminBumpsPromotionsAndAging(t *testing.T) {
	g := newAdmissionGate()
	now := time.Now()
	for _, tc := range []struct {
		reason string
		want   model.RunPriority
	}{
		{model.TagReleasePriorityReason, model.PriorityStandard},
		{"admin bump: deliberately urgent", model.PriorityHigh},
		{"promotion run", model.PriorityPromotion},
	} {
		r := &admissionReservation{gate: g, prepared: true, priorityLookupFailed: true, priority: model.PriorityHigh, priorityReason: tc.reason, queuedAt: now}
		if tc.reason == "promotion run" || tc.reason == model.TagReleasePriorityReason {
			r.priority = model.PriorityPromotion
		}
		if got := r.effectivePriority(now); got != tc.want {
			t.Fatalf("withheld priority for %q=%s", tc.reason, got)
		}
		// A capped tag waits as normal work and ages into high like any
		// normal run; bumps and promotions keep highest.
		aged := tc.want
		if aged == model.PriorityStandard {
			aged = model.PriorityPromotion
		}
		if got := r.effectivePriority(now.Add(time.Hour)); got != aged {
			t.Fatalf("aging for %q=%s, want %s", tc.reason, got, aged)
		}
	}
}

func TestPriorityWithheldTagRetainsExplicitAdminFallback(t *testing.T) {
	r := &admissionReservation{prepared: true, priorityLookupFailed: true, priority: model.PriorityHigh, priorityReason: model.TagReleasePriorityReason, priorityOverride: model.PriorityHigh, priorityOverrideReason: "intentional expedite"}
	run := r.priorityRun()
	if run.Priority != model.PriorityHigh || run.PriorityReason != "admin bump: intentional expedite" {
		t.Fatalf("withheld priority lost explicit fallback: %+v", run)
	}
}
