package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// Prepare under the gate lock so tests control every admission/release without
// sleeps, blocked worker goroutines or access to live reservation fields.
func priorityWaiter(g *weightedAdmission, seq int64, p model.RunPriority, at time.Time, size periapsis.Size) *admissionReservation {
	r := g.reserveRun(model.Run{ID: string(rune('a' + seq)), QueueSequence: seq, AdmissionSequence: seq, Priority: p, QueuedAt: at})
	g.mu.Lock()
	r.prepared = true
	r.preparedAt = g.clock()
	r.size = size
	r.weight = g.weights.of(size, g.capacity)
	g.grantLocked()
	g.mu.Unlock()
	return r
}

func TestPriorityAdmissionOrdersClassesThenFIFO(t *testing.T) {
	g := newWeightedAdmission(4)
	holder := g.reserve(1)
	release, err := holder.acquire(context.Background(), periapsis.XL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	normal := priorityWaiter(g, 2, model.PriorityNormal, now, periapsis.XL)
	high := priorityWaiter(g, 3, model.PriorityHigh, now, periapsis.XL)
	highest := priorityWaiter(g, 4, model.PriorityHighest, now, periapsis.XL)
	highest2 := priorityWaiter(g, 5, model.PriorityHighest, now, periapsis.XL)
	release()
	for i, r := range []*admissionReservation{highest, highest2, high, normal} {
		if !granted(g, r) {
			t.Fatalf("class/FIFO order failed at %d", i)
		}
		g.mu.Lock()
		if g.runs != 1 || g.used != 4 {
			t.Errorf("over-admission runs=%d used=%d", g.runs, g.used)
		}
		g.mu.Unlock()
		r.release()
	}
}

func TestPriorityAdmissionBackfillsOnlyWithinClassAndHonorsHeadWait(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	g := newWeightedAdmission(4)
	g.now = clock.Now
	g.maxHeadWait = 20 * time.Minute
	holder := g.reserve(1)
	release, err := holder.acquire(context.Background(), periapsis.M)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	head := priorityWaiter(g, 2, model.PriorityHigh, clock.now, periapsis.L)
	normal := priorityWaiter(g, 3, model.PriorityNormal, clock.now, periapsis.S)
	small := priorityWaiter(g, 4, model.PriorityHigh, clock.now, periapsis.S)
	if granted(g, head) || granted(g, normal) || !granted(g, small) {
		t.Fatal("backfill crossed priority class or lost same-class backfill")
	}
	small.release()
	clock.now = clock.now.Add(20 * time.Minute)
	later := priorityWaiter(g, 5, model.PriorityHigh, clock.now, periapsis.S)
	if granted(g, later) || granted(g, normal) {
		t.Fatal("head-wait bound did not drain higher class")
	}
	release()
	if !granted(g, head) {
		t.Fatal("head failed to start after drain")
	}
	head.release()
	later.release()
	normal.release()
}

func TestPriorityAdmissionStarvationReordersExistingWaiters(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	g := newWeightedAdmission(4)
	g.now = clock.Now
	holder := g.reserve(1)
	release, err := holder.acquire(context.Background(), periapsis.XL)
	if err != nil {
		t.Fatal(err)
	}
	normal := priorityWaiter(g, 2, model.PriorityNormal, clock.now.Add(-time.Hour+time.Nanosecond), periapsis.XL)
	highest := priorityWaiter(g, 3, model.PriorityHighest, clock.now, periapsis.XL)
	if g.waiters[0] != highest {
		t.Fatal("normal aged before threshold")
	}
	clock.now = clock.now.Add(time.Nanosecond)
	release()
	if !granted(g, normal) || granted(g, highest) {
		t.Fatal("starved normal did not precede newer highest work")
	}
	normal.release()
	highest.release()
}

func TestQueuePriorityToolsAdminOnlyAuditedAndGated(t *testing.T) {
	f := newControlFixture(t)
	ctx := context.Background()
	control, err := NewAPI(APIConfig{Runs: f.store, QueuePriorities: f.store, Enqueues: f.scheduler})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Shutdown(ctx) }()
	run, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "rc/v1", SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	admin := api.Actor{Identity: "admin@host", Admin: true}
	for _, actor := range []api.Actor{{Identity: "dev"}, {Identity: "scoped-admin", Admin: true, Scope: []string{"codeberg"}}} {
		for tool, raw := range map[string]string{"queue_priority_config": `{"rc_patterns":["rc/*"]}`, "queue_priority_get": `{}`, "run_priority": `{"run_id":"` + run.ID + `","priority":"highest","reason":"release blocker"}`} {
			if _, err = control.CallTool(ctx, actor, tool, json.RawMessage(raw)); !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s by %+v = %v", tool, actor, err)
			}
		}
	}
	got, err := f.store.Run(ctx, run.ID)
	if err != nil || got.Priority != model.PriorityNormal {
		t.Fatalf("nonadmin mutated priority: %+v %v", got, err)
	}
	if _, err = control.CallTool(ctx, admin, "queue_priority_config", json.RawMessage(`{"rc_patterns":["rc/*"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = control.CallTool(ctx, admin, "queue_priority_config", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing patterns=%v", err)
	}
	value, err := control.CallTool(ctx, admin, "run_priority", json.RawMessage(`{"run_id":"`+run.ID+`","priority":"highest","reason":"release blocker"}`))
	if err != nil {
		t.Fatal(err)
	}
	shown := value.(model.Run)
	if shown.Priority != model.PriorityHighest || !strings.Contains(shown.DisplayReason, "highest") || !strings.Contains(shown.DisplayReason, "release blocker") {
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
	g := newWeightedAdmission(2)
	scheduler := &Scheduler{store: f.store, admission: g, maxConcurrent: 1}
	holder := g.reserve(-1)
	release, err := holder.acquire(ctx, periapsis.XL)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	enqueue := func(ref string, kind model.RefKind) model.Run {
		t.Helper()
		r, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: kind, Ref: ref, SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
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
		reservation.size = periapsis.M
		reservation.weight = 2
		g.mu.Unlock()
	}
	if _, err = f.store.BumpRunPriority(ctx, "admin", second.ID, model.PriorityHigh, "RC blocker"); err != nil {
		t.Fatal(err)
	}
	if err = scheduler.refreshPriorities(ctx); err != nil {
		t.Fatal(err)
	}
	if g.waiters[0].runID != second.ID {
		t.Fatal("admin bump did not reorder an existing reservation")
	}
	tag := enqueue("v1", model.RefTag)
	if scheduler.dispatchSlotAvailable(3) {
		t.Fatal("test failed to saturate normal dispatch ceiling")
	}
	if available, err := scheduler.priorityDispatchAvailable(ctx, 3); err != nil || !available {
		t.Fatalf("release hidden behind dispatch ceiling: %v %v", available, err)
	}
	r, err := f.store.ClaimNextRun(ctx)
	if err != nil || r.ID != tag.ID {
		t.Fatal(err)
	}
	tagReservation := g.reserveRun(r)
	// The next release cannot pile up another same-class claim.
	enqueue("v2", model.RefTag)
	if available, err := scheduler.priorityDispatchAvailable(ctx, 4); err != nil || available {
		t.Fatalf("unbounded priority claims: %v %v", available, err)
	}
	g.mu.Lock()
	tagReservation.prepared = true
	tagReservation.size = periapsis.M
	tagReservation.weight = 2
	g.mu.Unlock()
	release()
	if !granted(g, tagReservation) {
		t.Fatal("release did not overtake ordinary reservations")
	}
	tagReservation.release()
	if !granted(g, g.reservations[second.QueueSequence]) {
		t.Fatal("admin bump lost after release")
	}
	for _, r := range g.reservations {
		r.cancel()
	}
}

func TestSchedulerReleaseCandidateOvertakesAlreadyClaimedBranchQueue(t *testing.T) {
	f := newGroupFixture(t, 2)
	f.engine.declare("running", "", periapsis.L) // 3/4 used
	f.engine.declare("ordinary-a", "", periapsis.L)
	f.engine.declare("ordinary-b", "", periapsis.L)
	f.engine.declare("rc/v1", "", periapsis.S)
	if _, err := f.store.SetQueuePriorityPatterns(t.Context(), "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	running := f.branch(t, f.terraform, "running", "a")
	f.start(t)
	if created := f.awaitCreated(t, 1); !created[running.ID] {
		t.Fatalf("initial creation=%v", created)
	}
	ordinaryA := f.branch(t, f.terraform, "ordinary-a", "b")
	ordinaryB := f.branch(t, f.beacon, "ordinary-b", "c")
	// Wait until an ordinary L has filled dispatch while awaiting weight.
	deadline := time.Now().Add(testStartTimeout(10 * time.Second))
	for {
		a := f.scheduler.AdmissionSnapshot(ordinaryA.QueueSequence)
		if a != nil && a.State == "waiting" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ordinary run never reached weighted wait")
		}
		time.Sleep(testTimeout(time.Millisecond))
	}
	rc := f.branch(t, f.beacon, "rc/v1", "d")
	if created := f.awaitCreated(t, 1); !created[rc.ID] {
		t.Fatalf("higher-class RC did not overtake branches: %v", created)
	}
	// Existing work keeps its resources; only RC fits the remaining weight.
	f.scheduler.admission.mu.Lock()
	used := f.scheduler.admission.used
	f.scheduler.admission.mu.Unlock()
	if used != 4 {
		t.Fatalf("priority bypassed weighted budget: used=%d", used)
	}
	f.engine.finish(t, rc.ID)
	f.engine.finish(t, running.ID)
	if created := f.awaitCreated(t, 1); !created[ordinaryA.ID] {
		t.Fatalf("ordinary FIFO lost: %v", created)
	}
	f.engine.finish(t, ordinaryA.ID)
	if created := f.awaitCreated(t, 1); !created[ordinaryB.ID] {
		t.Fatalf("remaining branch lost: %v", created)
	}
	f.engine.finish(t, ordinaryB.ID)
}
