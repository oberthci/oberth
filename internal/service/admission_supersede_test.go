package service

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// awaitAdmission waits until the run's reservation satisfies accept.
func (fixture *groupFixture) awaitAdmission(t *testing.T, run model.Run, what string, accept func(*model.AdmissionObservation) bool) *model.AdmissionObservation {
	t.Helper()
	deadline := time.Now().Add(testStartTimeout(15 * time.Second))
	for {
		snapshot := fixture.scheduler.AdmissionSnapshot(run.QueueSequence)
		if accept(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never reached %s: %#v", run.ID, what, snapshot)
		}
		time.Sleep(testTimeout(5 * time.Millisecond))
	}
}

// diskValve holds grants for lifecycle tests.
func (fixture *groupFixture) diskValve(t *testing.T) func() {
	t.Helper()
	blockForLifecycleTest(fixture.scheduler.admission)
	return func() { unblockLifecycleTest(fixture.scheduler.admission); fixture.scheduler.signalQueue() }
}

// A waiting run that a newer push supersedes must withdraw its reservation
// at once: the durable record turns interrupted, and the runs it was ahead
// of are admitted as soon as nothing else holds them, without a restart.
func TestSupersedingAWaitingRunReleasesItsReservationWithoutDrain(t *testing.T) {
	fixture := newGroupFixture(t, 0)
	lift := fixture.diskValve(t)
	head := fixture.branch(t, fixture.terraform, "feat-kv", "b")
	other := fixture.branch(t, fixture.exampleService, "small", "d")
	fixture.start(t)
	fixture.awaitAdmission(t, head, "pressure-held head", func(snapshot *model.AdmissionObservation) bool {
		return snapshot != nil && snapshot.State == "waiting"
	})
	newer := fixture.branch(t, fixture.terraform, "feat-kv", "e")
	superseded := fixture.awaitTerminal(t, head.ID)
	if superseded.Status != model.RunInterrupted || superseded.SupersededBy != newer.ID {
		t.Fatalf("superseded head = status %s superseded_by %q", superseded.Status, superseded.SupersededBy)
	}
	if snapshot := fixture.scheduler.AdmissionSnapshot(head.QueueSequence); snapshot != nil {
		t.Fatalf("superseded head still holds a reservation: %#v", snapshot)
	}
	lift()
	created := fixture.awaitCreated(t, 2)
	if !created[other.ID] || !created[newer.ID] || fixture.engine.wasCreated(head.ID) {
		t.Fatalf("after the supersede the gate admitted %v", created)
	}
	fixture.engine.finish(t, other.ID)
	fixture.engine.finish(t, newer.ID)
	fixture.awaitTerminal(t, newer.ID)
	fixture.engine.assertNoViolations(t)
	if pending, err := fixture.store.PendingRunCancellations(t.Context()); err != nil || len(pending) != 0 {
		t.Fatalf("pending cancellations after the supersede = %#v, %v", pending, err)
	}
}
