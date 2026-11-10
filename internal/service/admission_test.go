package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// admissionAcquireBudget bounds an acquire a test expects to succeed, so a
// regression that reintroduces serialization fails with the run number that
// was starved instead of blocking until the package timeout kills the suite.
const admissionAcquireBudget = 2 * time.Second

func acquireWithinBudget(t *testing.T, reservation *admissionReservation, size pipeline.Size) (func(), error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testStartTimeout(admissionAcquireBudget))
	defer cancel()
	return reservation.acquire(ctx, size)
}

// Oberth has no run ceiling (Argo parallelism bounds running Workflows):
// every prepared run is granted at once whatever its historical size tier,
// unless the disk floor, host pressure or the administrator hold says
// otherwise.
func TestAdmissionHasNoRunCeiling(t *testing.T) {
	gate := newAdmissionGate()
	var releases []func()
	for sequence, size := range []pipeline.Size{"", pipeline.S, pipeline.M, pipeline.L, pipeline.XL, pipeline.XL} {
		release, err := acquireWithinBudget(t, gate.reserve(int64(sequence+1)), size)
		if err != nil {
			t.Fatalf("run %d (%q) waited for a ceiling: %v", sequence+1, size, err)
		}
		releases = append(releases, release)
	}
	if gate.runs != len(releases) {
		t.Fatalf("runs=%d, want %d", gate.runs, len(releases))
	}
	for _, release := range releases {
		release()
	}
	if gate.runs != 0 {
		t.Fatalf("gate leaked runs=%d", gate.runs)
	}
}

// Waiters held back by the disk floor keep claim order: when it clears, the
// grant pass admits them in FIFO order, and nothing later overtakes a head
// that is still preparing.
func TestAdmissionIsFIFO(t *testing.T) {
	gate := newAdmissionGate()
	blockOnDiskFloor(gate)
	xl := gate.reserve(2)
	small := gate.reserve(3)
	xlAcquired := make(chan func(), 1)
	smallAcquired := make(chan func(), 1)
	go acquireForTest(xl, pipeline.XL, xlAcquired)
	awaitWaiting(t, xl)
	go acquireForTest(small, pipeline.S, smallAcquired)
	awaitWaiting(t, small)
	gate.mu.Lock()
	order := []*admissionReservation{gate.waiters[0], gate.waiters[1]}
	gate.mu.Unlock()
	if order[0] != xl || order[1] != small {
		t.Fatal("waiters lost claim order")
	}
	clearDiskFloor(gate)
	awaitAcquired(t, xlAcquired)()
	awaitAcquired(t, smallAcquired)()
}

func TestAdmissionReservesClaimOrderBeforePreparationCompletes(t *testing.T) {
	gate := newAdmissionGate()
	first := gate.reserve(1)
	second := gate.reserve(2)
	secondAcquired := make(chan func(), 1)
	go acquireForTest(second, pipeline.S, secondAcquired)
	// The first claim is still preparing: the prepared run behind it waits.
	assertNotAcquired(t, secondAcquired)

	releaseFirst, err := first.acquire(context.Background(), pipeline.XL)
	if err != nil {
		t.Fatal(err)
	}
	releaseSecond := awaitAcquired(t, secondAcquired)
	releaseFirst()
	releaseSecond()
}

func acquireForTest(reservation *admissionReservation, size pipeline.Size, acquired chan<- func()) {
	release, err := reservation.acquire(context.Background(), size)
	if err == nil {
		acquired <- release
	}
}

func assertNotAcquired(t *testing.T, acquired <-chan func()) {
	t.Helper()
	select {
	case release := <-acquired:
		release()
		t.Fatal("reservation acquired unexpectedly")
	case <-time.After(testTimeout(20 * time.Millisecond)):
	}
}

func awaitAcquired(t *testing.T, acquired <-chan func()) func() {
	t.Helper()
	select {
	case release := <-acquired:
		return release
	case <-time.After(testStartTimeout(time.Second)):
		t.Fatal("reservation did not acquire")
		return nil
	}
}

// abandon withdraws only an ungranted reservation. A waiting head that is
// abandoned returns errAdmissionAbandoned to its owner and leaves its place
// to the run queued behind it; a granted reservation stays with its owner
// until the owner releases it, and a preparing one fails its later acquire.
func TestAdmissionAbandonWithdrawsOnlyUngrantedReservations(t *testing.T) {
	gate := newAdmissionGate()
	holder := gate.reserve(1)
	releaseHolder, err := holder.acquire(context.Background(), pipeline.L)
	if err != nil {
		t.Fatal(err)
	}
	blockOnDiskFloor(gate)
	head := gate.reserve(2)
	headErr := make(chan error, 1)
	go func() {
		_, err := head.acquire(context.Background(), pipeline.XL)
		headErr <- err
	}()
	awaitWaiting(t, head)
	gate.mu.Lock()
	gate.grantLocked()
	gate.mu.Unlock()
	behind := gate.reserve(3)
	acquired := make(chan func(), 1)
	go acquireForTest(behind, pipeline.S, acquired)
	assertNotAcquired(t, acquired)

	if gate.abandon(1) {
		t.Fatal("a granted reservation was abandoned")
	}
	if !gate.abandon(2) {
		t.Fatal("the waiting XL head was not abandoned")
	}
	select {
	case err := <-headErr:
		if !errors.Is(err, errAdmissionAbandoned) {
			t.Fatalf("abandoned waiter returned %v", err)
		}
	case <-time.After(testStartTimeout(admissionAcquireBudget)):
		t.Fatal("abandoned waiter never woke")
	}
	releaseHolder()
	clearDiskFloor(gate)
	releaseBehind := awaitAcquired(t, acquired)
	if gate.abandon(2) {
		t.Fatal("an abandoned reservation was abandoned twice")
	}

	preparing := gate.reserve(4)
	if !gate.abandon(4) {
		t.Fatal("a preparing reservation was not abandoned")
	}
	if _, err := preparing.acquire(context.Background(), pipeline.S); !errors.Is(err, errAdmissionAbandoned) {
		t.Fatalf("acquire after abandon = %v", err)
	}
	releaseBehind()
	releaseHolder()
	if gate.runs != 0 || len(gate.waiters) != 0 || len(gate.reservations) != 0 {
		t.Fatalf("gate leaked state: runs=%d waiters=%d reservations=%d", gate.runs, len(gate.waiters), len(gate.reservations))
	}
}
