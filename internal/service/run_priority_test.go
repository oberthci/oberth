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
		trigger := "push"
		if kind == model.RefTag {
			trigger = "tag"
		}
		r, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: kind, Ref: ref, SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: trigger})
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

func TestPriorityAdmissionAgesHighBeforeNewerNormal(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	g := newWeightedAdmission(4)
	g.now = clock.Now
	holder := g.reserve(1)
	release, err := holder.acquire(t.Context(), periapsis.XL)
	if err != nil {
		t.Fatal(err)
	}
	high := priorityWaiter(g, 2, model.PriorityHigh, clock.now, periapsis.XL)
	clock.now = clock.now.Add(time.Minute)
	normal := priorityWaiter(g, 3, model.PriorityNormal, clock.now, periapsis.XL)
	clock.now = clock.now.Add(time.Hour)
	release()
	if !granted(g, high) || granted(g, normal) {
		t.Fatal("aged normal leapfrogged older high")
	}
	high.release()
	if !granted(g, normal) {
		t.Fatal("aged normal did not follow aged high")
	}
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
	enqueued, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(ctx)
	if err != nil || run.ID != enqueued.ID {
		t.Fatalf("claim=%+v %v", run, err)
	}
	r := f.scheduler.admission.reserveRun(run)
	release, err := r.acquire(ctx, periapsis.M)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if run.StartedAt != nil {
		t.Fatal("test needs a grant without durable started_at")
	}
	_, err = control.CallTool(ctx, api.Actor{Identity: "admin", Admin: true}, "run_priority", json.RawMessage(`{"run_id":"`+run.ID+`","priority":"highest","reason":"too late"}`))
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
	if shown := presentationRunAt(run, now); shown.DisplayReason != "Priority high: RC." || shown.PriorityReason != "RC" {
		t.Fatalf("active display=%+v", shown)
	}
	run.Status, run.Phase, run.Reason = model.RunInterrupted, model.PhaseCancelled, "cancelled by admin: obsolete"
	if shown := presentationRunAt(run, now); shown.DisplayReason != run.Reason || shown.PriorityReason != "RC" {
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
			f.engine.declare("running", "", periapsis.L)
			f.engine.declare("waiting", "", periapsis.L)
			f.engine.declare("rc/v1", "", periapsis.S)
			if _, err := f.store.SetQueuePriorityPatterns(t.Context(), "admin", []string{"rc/*"}); err != nil {
				t.Fatal(err)
			}
			running := f.branch(t, f.terraform, "running", "a")
			f.start(t)
			f.awaitCreated(t, 1)
			waiting := f.branch(t, f.terraform, "waiting", "b")
			deadline := time.Now().Add(testStartTimeout(10 * time.Second))
			for {
				if snapshot := f.scheduler.AdmissionSnapshot(waiting.QueueSequence); snapshot != nil && snapshot.State == "waiting" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("ordinary run never waited for weight")
				}
				time.Sleep(testTimeout(5 * time.Millisecond))
			}
			if operation == "refresh" {
				busy.refreshBusy.Store(true)
			} else {
				busy.peekBusy.Store(true)
			}
			f.scheduler.NotifyQueue()
			select {
			case got := <-busy.observed:
				if got != operation {
					t.Fatalf("busy read=%s", got)
				}
			case <-time.After(testStartTimeout(10 * time.Second)):
				t.Fatal("busy read was not exercised")
			}
			rc := f.branch(t, f.terraform, "rc/v1", "c")
			if created := f.awaitCreated(t, 1); !created[rc.ID] {
				t.Fatalf("dispatch did not recover after busy %s: %v", operation, created)
			}
			f.engine.finish(t, rc.ID)
			f.engine.finish(t, running.ID)
			if created := f.awaitCreated(t, 1); !created[waiting.ID] {
				t.Fatalf("ordinary waiter did not resume: %v", created)
			}
			f.engine.finish(t, waiting.ID)
			f.awaitTerminal(t, waiting.ID)
		})
	}
}

func TestPriorityReleaseStreamPreservesAgingAndResourceBounds(t *testing.T) {
	clock := &fakeClock{now: time.Now().UTC()}
	g := newWeightedAdmission(4)
	g.now, g.maxHeadWait, g.maxRuns = clock.Now, 20*time.Minute, 1
	origin := clock.Now()
	holder := g.reserve(1)
	release, err := holder.acquire(t.Context(), periapsis.XL)
	if err != nil {
		t.Fatal(err)
	}
	normal := priorityWaiter(g, 2, model.PriorityNormal, origin, periapsis.XL)
	clock.now = origin.Add(45 * time.Minute)
	tag := priorityWaiter(g, 3, model.PriorityHighest, clock.Now(), periapsis.XL)
	promotion := priorityWaiter(g, 4, model.PriorityHighest, clock.Now(), periapsis.XL)
	if granted(g, tag) || granted(g, normal) {
		t.Fatal("priority preempted existing resource grant")
	}
	release()
	if !granted(g, tag) || granted(g, normal) || granted(g, promotion) {
		t.Fatal("release did not lead ordinary work")
	}
	tag.release()
	if !granted(g, promotion) || granted(g, normal) {
		t.Fatal("promotion lost highest-class FIFO")
	}
	clock.now = origin.Add(time.Hour - time.Nanosecond)
	var later []*admissionReservation
	for seq := int64(5); seq < 25; seq++ {
		later = append(later, priorityWaiter(g, seq, model.PriorityHighest, clock.Now(), periapsis.XL))
	}
	g.mu.Lock()
	before := g.waiters[0]
	g.mu.Unlock()
	if before == normal {
		t.Fatal("normal aged before the 60-minute bound")
	}
	clock.now = origin.Add(time.Hour)
	promotion.release()
	if !granted(g, normal) {
		t.Fatal("continuous releases starved aged normal")
	}
	g.mu.Lock()
	used, runs := g.used, g.runs
	g.mu.Unlock()
	if used != 4 || runs != 1 {
		t.Fatalf("capacity exceeded: weight=%d runs=%d", used, runs)
	}
	normal.release()
	for _, candidate := range later {
		if !granted(g, candidate) {
			t.Fatal("highest-class FIFO changed after starvation guard")
		}
		candidate.release()
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
		if err != nil || branch.Priority != model.PriorityNormal || branch.RefKind != model.RefBranch {
			t.Fatalf("branch transport claimed release urgency: %+v %v", branch, err)
		}
	}
	release, err := f.scheduler.AdmitRelease(ctx, ReleaseRequest{EventID: "priority-tag", Repository: f.repo, Tag: "v1.2.3", ObjectSHA: strings.Repeat("b", 40), CommitSHA: strings.Repeat("c", 40), Actor: "pusher"})
	if err != nil || release.Priority != model.PriorityHighest || release.RefKind != model.RefTag || !release.Release {
		t.Fatalf("genuine ungranted tag failed priority admission: %+v %v", release, err)
	}
	next, err := f.store.NextQueuedRun(ctx)
	if err != nil || next.ID != release.ID {
		t.Fatalf("release remained behind ordinary branch queue: %+v %v", next, err)
	}
}
