package service

import (
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
	behind := gate.reserveRun(model.Run{ID: "behind", RepoID: 1, Ref: "feature/b", QueueSequence: 2, AdmissionSequence: 2})
	refired := gate.reserveRun(model.Run{ID: "refired", RepoID: 1, Ref: "feature/a", QueueSequence: 3, AdmissionSequence: 1})
	defer behind.cancel()
	defer refired.cancel()
	gate.mu.Lock()
	for _, reservation := range []*admissionReservation{behind, refired} {
		reservation.prepared, reservation.size, reservation.weight = true, periapsis.L, gate.capacity
	}
	gate.mu.Unlock()
	scheduler := &Scheduler{admission: gate}
	head := scheduler.AdmissionSnapshot(3)
	if head == nil || !head.HeadOfQueue || head.Position != 1 || head.BlockedBy == nil || head.BlockedBy.RunID != "refired" {
		t.Fatalf("re-fired head snapshot = %#v", head)
	}
	tail := scheduler.AdmissionSnapshot(2)
	if tail == nil || tail.HeadOfQueue || tail.Position != 2 || tail.BlockedBy == nil || tail.BlockedBy.RunID != "refired" {
		t.Fatalf("queued-behind snapshot = %#v", tail)
	}
	state := scheduler.AdmissionState()
	if len(state.Reservations) != 2 || state.Reservations[0].RunID != "refired" || state.Reservations[1].RunID != "behind" || state.Blocker == nil || state.Blocker.RunID != "refired" {
		t.Fatalf("inherited FIFO admission state = %#v", state)
	}
	if refired.claimedAt.IsZero() || refired.repoID != 1 || refired.ref != "feature/a" {
		t.Fatalf("re-fired reservation lost claim metadata: %#v", refired)
	}
}
