package service

import "testing"

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
