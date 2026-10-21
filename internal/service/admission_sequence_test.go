package service

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestAdmissionReservationUsesInheritedFIFOOrderAndUniqueRunKeys(t *testing.T) {
	gate := newWeightedAdmissionForJobs(2)
	queuedBehind := gate.reserveInOrder(2, 2)
	refired := gate.reserveInOrder(3, 1)
	defer queuedBehind.cancel()
	defer refired.cancel()
	gate.mu.Lock()
	if len(gate.waiters) != 2 || gate.waiters[0] != refired || gate.waiters[1] != queuedBehind {
		gate.mu.Unlock()
		t.Fatal("re-fired run did not regain its admission place")
	}
	gate.mu.Unlock()
	scheduler := &Scheduler{admission: gate}
	if scheduler.AdmissionSnapshot(2) == nil || scheduler.AdmissionSnapshot(3) == nil {
		t.Fatal("shared admission order replaced a run's unique snapshot key")
	}
}

func TestAdmissionSnapshotAndStateKeepRefiredHeadIdentity(t *testing.T) {
	gate := newWeightedAdmissionForJobs(2)
	held := gate.reserve(1)
	release, err := held.acquire(context.Background(), periapsis.XL)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	behind := gate.reserveRun(model.Run{ID: "behind", RepoID: 1, Ref: "feature/b", QueueSequence: 3, AdmissionSequence: 3})
	refired := gate.reserveRun(model.Run{ID: "refired", RepoID: 1, Ref: "feature/a", QueueSequence: 4, AdmissionSequence: 2})
	defer behind.cancel()
	defer refired.cancel()
	gate.mu.Lock()
	for _, reservation := range []*admissionReservation{behind, refired} {
		reservation.prepared, reservation.size, reservation.weight = true, periapsis.L, gate.capacity
	}
	gate.mu.Unlock()
	scheduler := &Scheduler{admission: gate}
	head := scheduler.AdmissionSnapshot(4)
	if head == nil || !head.HeadOfQueue || head.Position != 1 || head.BlockedBy == nil || head.BlockedBy.RunID != "refired" {
		t.Fatalf("re-fired head snapshot = %#v", head)
	}
	tail := scheduler.AdmissionSnapshot(3)
	if tail == nil || tail.HeadOfQueue || tail.Position != 2 || tail.BlockedBy == nil || tail.BlockedBy.RunID != "refired" {
		t.Fatalf("queued-behind snapshot = %#v", tail)
	}
	state := scheduler.AdmissionState()
	if len(state.Reservations) != 3 || state.Reservations[1].RunID != "refired" || state.Reservations[2].RunID != "behind" || state.Blocker == nil || state.Blocker.RunID != "refired" {
		t.Fatalf("inherited FIFO admission state = %#v", state)
	}
	if refired.claimedAt.IsZero() || refired.repoID != 1 || refired.ref != "feature/a" {
		t.Fatalf("re-fired reservation lost claim metadata: %#v", refired)
	}
}
