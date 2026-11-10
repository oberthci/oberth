package service

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// groupRequest is one claimed run of repository repoID. The size and the
// retired concurrency group are ignored: admission reads neither.
func groupRequest(runID string, repoID int64, _ pipeline.Size, _ string) admissionRequest {
	return admissionRequest{RunID: runID, RepoID: repoID}
}

func acquireRequestForTest(reservation *admissionReservation, request admissionRequest, acquired chan<- func()) {
	release, err := reservation.acquireRequest(context.Background(), request)
	if err == nil {
		acquired <- release
	}
}

func acquireRequestWithinBudget(t *testing.T, reservation *admissionReservation, request admissionRequest) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testStartTimeout(admissionAcquireBudget))
	defer cancel()
	release, err := reservation.acquireRequest(ctx, request)
	if err != nil {
		t.Fatalf("run %s (repo %d) was not admitted: %v", request.RunID, request.RepoID, err)
	}
	return release
}

// awaitWaiting waits until the acquiring goroutine has prepared the
// reservation, which is when the gate makes its admission decision for it, and
// then asserts that decision: waiting, neither granted nor canceled. Nothing
// else can grant it until the test itself releases or cancels a reservation,
// so this observes the gate's decision instead of inferring it from a quiet
// channel over a timer.
func awaitWaiting(t *testing.T, reservation *admissionReservation) {
	t.Helper()
	gate := reservation.gate
	deadline := time.Now().Add(testStartTimeout(admissionAcquireBudget))
	for {
		gate.mu.Lock()
		prepared, granted, canceled := reservation.prepared, reservation.granted, reservation.canceled
		gate.mu.Unlock()
		if prepared || canceled {
			if granted || canceled {
				t.Fatalf("reservation %d: granted=%v canceled=%v; want it waiting", reservation.sequence, granted, canceled)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reservation %d was never prepared", reservation.sequence)
		}
		time.Sleep(testTimeout(time.Millisecond))
	}
}

// blockOnDiskFloor holds every new grant on the disk floor, the admission
// guard tests use to keep a run waiting now that Oberth has no run ceiling.
func blockOnDiskFloor(gate *admissionGate) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.diskFloor = &diskFloorCheck{floorBytes: 100, cachedFree: 0, cacheTime: time.Now(), cacheTTL: time.Hour}
}

// clearDiskFloor lifts blockOnDiskFloor and runs a grant pass.
func clearDiskFloor(gate *admissionGate) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.diskFloor = nil
	gate.grantLocked()
}

// assertWaiterOrder checks the gate's admission order, re-sorted for the
// current clock as every grant pass does.
func assertWaiterOrder(t *testing.T, gate *admissionGate, want ...*admissionReservation) {
	t.Helper()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.sortWaitersLocked()
	if len(gate.waiters) < len(want) {
		t.Fatalf("waiters=%d, want at least %d", len(gate.waiters), len(want))
	}
	for i, r := range want {
		if gate.waiters[i] != r {
			t.Fatalf("admission order failed at %d (priority %s)", i, r.priority)
		}
	}
}
