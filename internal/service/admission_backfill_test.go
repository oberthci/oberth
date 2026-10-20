package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type fakeClock struct{ now time.Time }

func (clock *fakeClock) Now() time.Time { return clock.now }

func backfillGate(clock *fakeClock) *weightedAdmission {
	gate := newWeightedAdmissionForJobsWeights(8, DefaultSizeWeights) // budget 16
	gate.maxHeadWait = 20 * time.Minute
	gate.now = clock.Now
	gate.headEvents = make(chan headEvent, 64)
	return gate
}

// pending starts an acquire that may wait and returns its reservation.
func pending(t *testing.T, gate *weightedAdmission, sequence int64, request admissionRequest) *admissionReservation {
	t.Helper()
	reservation := gate.reserve(sequence)
	go func() { _, _ = reservation.acquireRequest(context.Background(), request) }()
	deadline := time.Now().Add(admissionAcquireBudget)
	for {
		gate.mu.Lock()
		settled := reservation.prepared || reservation.granted
		gate.mu.Unlock()
		if settled {
			return reservation
		}
		if time.Now().After(deadline) {
			t.Fatalf("reservation %d never prepared", sequence)
		}
		time.Sleep(time.Millisecond)
	}
}

func granted(gate *weightedAdmission, reservation *admissionReservation) bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return reservation.granted
}

func kick(gate *weightedAdmission) {
	gate.mu.Lock()
	gate.grantLocked()
	gate.mu.Unlock()
}

// An XL head waiting for its drain no longer idles the host: later S and M
// runs that fit the spare weight are admitted beside it, marked backfilled,
// and the head and the runs behind it say so.
func TestXLHeadBackfillsRunsThatFit(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)}
	gate := backfillGate(clock)
	scheduler := &Scheduler{admission: gate}
	release := acquireRequestWithinBudget(t, gate.reserve(1), admissionRequest{RunID: "running-l", Size: periapsis.L})
	defer release()
	head := pending(t, gate, 2, admissionRequest{RunID: "ebpf", RepoID: 5, Repo: "cloudtaser-ebpf", Ref: "promotion/main/1", Size: periapsis.XL,
		RefKind: model.RefBranch, SHA: strings.Repeat("a", 40)})
	small := pending(t, gate, 3, admissionRequest{RunID: "docs", Size: periapsis.S})
	medium := pending(t, gate, 4, admissionRequest{RunID: "cli", Size: periapsis.M})
	if granted(gate, head) || !granted(gate, small) || !granted(gate, medium) {
		t.Fatalf("head %v small %v medium %v; want the S and M backfilled beside the waiting XL", head.granted, small.granted, medium.granted)
	}
	select {
	case event := <-gate.headEvents:
		if event.clear || event.key.RepoID != 5 || !event.since.Equal(clock.now) {
			t.Fatalf("head event = %+v", event)
		}
	default:
		t.Fatal("the head's wait origin was not emitted for persistence")
	}
	state := scheduler.AdmissionState()
	if state.MaxHeadWaitSeconds != 1200 || state.Blocker == nil || !state.Blocker.Backfilling {
		t.Fatalf("state = %+v", state)
	}
	backfilled := 0
	for _, reservation := range state.Reservations {
		if reservation.Backfilled {
			backfilled++
		}
	}
	if backfilled != 2 {
		t.Fatalf("backfilled reservations = %d, want 2", backfilled)
	}
	clock.now = clock.now.Add(7 * time.Minute)
	headMessage := waitingAdmissionMessage(scheduler.AdmissionSnapshot(2))
	if !strings.Contains(headMessage, "head waiting 7m/20m, backfilling") {
		t.Fatalf("head message = %q", headMessage)
	}
	behind := pending(t, gate, 5, admissionRequest{RunID: "huge", Size: periapsis.XL})
	if message := waitingAdmissionMessage(scheduler.AdmissionSnapshot(5)); !strings.Contains(message, "head waiting 7m/20m, backfilling") {
		t.Fatalf("behind message = %q", message)
	}
	behind.cancel()
}

// No starvation: under a steady stream of small runs the head starts within
// maxHeadWait plus the longest admitted backfill's duration, because nothing
// is backfilled once the bound has passed, and runs whose history predicts
// they would end after the bound are not backfilled at all.
func TestHeadStartsWithinTheBoundUnderASteadyStream(t *testing.T) {
	start := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start}
	gate := backfillGate(clock)
	first := gate.reserve(1)
	acquireRequestWithinBudget(t, first, admissionRequest{RunID: "first", Size: periapsis.M})
	head := pending(t, gate, 2, admissionRequest{RunID: "head", Size: periapsis.XL})

	type running struct {
		reservation *admissionReservation
		ends        time.Time
	}
	const duration = 5 * time.Minute
	active := []running{{first, start.Add(6 * time.Minute)}}
	sequence := int64(3)
	headStarted := time.Time{}
	lastBackfill := time.Time{}
	for minute := 0; minute <= 60 && headStarted.IsZero(); minute++ {
		clock.now = start.Add(time.Duration(minute) * time.Minute)
		kept := active[:0]
		for _, run := range active {
			if !clock.now.Before(run.ends) {
				run.reservation.release()
				continue
			}
			kept = append(kept, run)
		}
		active = kept
		kick(gate)
		if granted(gate, head) {
			headStarted = clock.now
			break
		}
		// One new small run arrives every minute; half of them have history.
		estimate := time.Duration(0)
		if minute%2 == 0 {
			estimate = duration
		}
		reservation := pending(t, gate, sequence, admissionRequest{RunID: "s", Size: periapsis.S, Estimate: estimate})
		sequence++
		if granted(gate, reservation) {
			if estimate > 0 && clock.now.Add(estimate).After(start.Add(gate.maxHeadWait)) {
				t.Fatalf("at %s a run predicted to end after the bound was backfilled", clock.now.Sub(start))
			}
			if !clock.now.Before(start.Add(gate.maxHeadWait)) {
				t.Fatalf("at %s a run was backfilled after the bound", clock.now.Sub(start))
			}
			lastBackfill = clock.now
			active = append(active, running{reservation, clock.now.Add(duration)})
		}
	}
	if headStarted.IsZero() {
		t.Fatal("the head never started")
	}
	if lastBackfill.IsZero() {
		t.Fatal("nothing was backfilled while the head waited")
	}
	if limit := start.Add(gate.maxHeadWait + duration); headStarted.After(limit) {
		t.Fatalf("head started at %s, after the bound plus the longest backfill (%s)", headStarted.Sub(start), limit.Sub(start))
	}
}

// The head's wait origin survives a restart: a new gate given the recorded
// origin is already past the bound and backfills nothing.
func TestRestartKeepsTheHeadWaitOrigin(t *testing.T) {
	origin := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: origin.Add(21 * time.Minute)}
	gate := backfillGate(clock)
	scheduler := &Scheduler{admission: gate}
	release := acquireRequestWithinBudget(t, gate.reserve(1), admissionRequest{RunID: "running", Size: periapsis.M})
	defer release()
	head := pending(t, gate, 2, admissionRequest{RunID: "head", Size: periapsis.XL, HeadSince: origin})
	small := pending(t, gate, 3, admissionRequest{RunID: "small", Size: periapsis.S})
	if granted(gate, head) || granted(gate, small) {
		t.Fatal("a restarted gate backfilled past the recorded bound")
	}
	if message := waitingAdmissionMessage(scheduler.AdmissionSnapshot(3)); !strings.Contains(message, "draining for head") {
		t.Fatalf("message = %q", message)
	}
	select {
	case event := <-gate.headEvents:
		t.Fatalf("a recorded origin was re-emitted: %+v", event)
	default:
	}
	small.cancel()
}

// With backfill disabled (the zero value) the gate is strict FIFO as before.
func TestZeroMaxHeadWaitIsStrictFIFO(t *testing.T) {
	gate := newWeightedAdmissionForJobsWeights(8, DefaultSizeWeights)
	release := acquireRequestWithinBudget(t, gate.reserve(1), admissionRequest{RunID: "running", Size: periapsis.M})
	defer release()
	head := pending(t, gate, 2, admissionRequest{RunID: "head", Size: periapsis.XL})
	small := pending(t, gate, 3, admissionRequest{RunID: "small", Size: periapsis.S})
	if granted(gate, head) || granted(gate, small) {
		t.Fatal("strict FIFO admitted a run behind a waiting head")
	}
	small.cancel()
	head.cancel()
}

// The durable origin is read back for a requeued copy of the same commit.
func TestAdmissionHistoryReadsTheDurableOrigin(t *testing.T) {
	fixture := newConcurrencyFixture(t, periapsis.M, 1)
	ctx := context.Background()
	sha := strings.Repeat("b", 40)
	origin := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Microsecond)
	if err := fixture.store.RecordAdmissionHeadWait(ctx, fixture.repo.ID, model.RefBranch, "feature/kvm", sha, origin); err != nil {
		t.Fatal(err)
	}
	since, _ := fixture.scheduler.admissionHistory(ctx, model.Run{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/kvm", SHA: sha})
	if !since.Equal(origin) {
		t.Fatalf("origin = %s, want %s", since, origin)
	}
}

// A FIFO run claimed behind the head and passed over as a backfill candidate
// (here: predicted to end after the bound) must not freeze claiming for the
// rest of the window; a claim still preparing does.
func TestPassedOverCandidateDoesNotFreezeBackfillClaims(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)}
	gate := backfillGate(clock)
	release := acquireRequestWithinBudget(t, gate.reserve(1), admissionRequest{RunID: "running", Size: periapsis.L})
	defer release()
	head := pending(t, gate, 2, admissionRequest{RunID: "head", Size: periapsis.XL})
	long := pending(t, gate, 3, admissionRequest{RunID: "long", Size: periapsis.M, Estimate: time.Hour})
	if granted(gate, head) || granted(gate, long) {
		t.Fatal("the long candidate or the head was admitted")
	}
	if !gate.spareWeightForClaim(3) {
		t.Fatal("a passed-over candidate froze backfill claims")
	}
	preparing := gate.reserve(4)
	if gate.spareWeightForClaim(4) {
		t.Fatal("claimed past a claim still preparing")
	}
	preparing.cancel()
	clock.now = clock.now.Add(21 * time.Minute)
	if gate.spareWeightForClaim(3) {
		t.Fatal("claimed past the head after its bound")
	}
	long.cancel()
	head.cancel()
}

// With backfill disabled nothing is marked or persisted as a head origin.
func TestNoHeadOriginWithoutBackfill(t *testing.T) {
	gate := newWeightedAdmissionForJobsWeights(8, DefaultSizeWeights)
	gate.headEvents = make(chan headEvent, 4)
	release := acquireRequestWithinBudget(t, gate.reserve(1), admissionRequest{RunID: "running", Size: periapsis.M})
	defer release()
	head := pending(t, gate, 2, admissionRequest{RunID: "head", Size: periapsis.XL, RepoID: 1, SHA: strings.Repeat("a", 40)})
	gate.mu.Lock()
	marked := !head.headSince.IsZero()
	gate.mu.Unlock()
	if marked || len(gate.headEvents) != 0 {
		t.Fatal("a head origin was recorded with backfill disabled")
	}
	head.cancel()
}

// A backfilled grant that is parked on VM capacity and granted again with
// no head waiting is no longer reported as backfilled.
func TestRegrantWithoutAHeadClearsBackfilled(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)}
	gate := backfillGate(clock)
	releaseRunning := acquireRequestWithinBudget(t, gate.reserve(1), admissionRequest{RunID: "running", Size: periapsis.M})
	head := pending(t, gate, 2, admissionRequest{RunID: "head", Size: periapsis.XL})
	small := pending(t, gate, 3, admissionRequest{RunID: "small", Size: periapsis.S})
	if !granted(gate, small) || !small.backfilled {
		t.Fatal("the S run was not backfilled")
	}
	if err := small.parkVM(); err != nil {
		t.Fatal(err)
	}
	head.cancel()
	releaseRunning()
	go func() {
		_, _ = small.acquireRequest(context.Background(), admissionRequest{RunID: "small", Size: periapsis.S})
	}()
	deadline := time.Now().Add(admissionAcquireBudget)
	for !granted(gate, small) {
		if time.Now().After(deadline) {
			t.Fatal("the parked run was not granted again")
		}
		time.Sleep(time.Millisecond)
	}
	gate.mu.Lock()
	backfilled := small.backfilled
	gate.mu.Unlock()
	if backfilled {
		t.Fatal("a grant with no head waiting is still marked backfilled")
	}
}

func TestBackfillWaitText(t *testing.T) {
	for blocker, want := range map[model.AdmissionBlocker]string{
		{Backfilling: true, HeadWaitSeconds: 45, MaxHeadWaitSeconds: 1200}: "head waiting 45s/20m, backfilling",
		{Backfilling: true, HeadWaitSeconds: -3, MaxHeadWaitSeconds: 1200}: "head waiting 0s/20m, backfilling",
		{Draining: true, HeadWaitSeconds: 1500, MaxHeadWaitSeconds: 1200}:  "head waited 25m of 20m, draining for head",
		{HeadWaitSeconds: 10}: "",
	} {
		if got := blocker.Backfill(); got != want {
			t.Fatalf("%+v = %q, want %q", blocker, got, want)
		}
	}
	if durationClass(model.Run{Release: true, Trigger: "tag"}) != "release" || durationClass(model.Run{Trigger: "push"}) != "branch" ||
		durationClass(model.Run{Trigger: "promotion"}) != "promotion" {
		t.Fatal("duration classes")
	}
}
