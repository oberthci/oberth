package service

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// A waiting run that a newer push supersedes must withdraw its reservation
// at once: the durable record turns interrupted, and the runs it was ahead
// of are admitted as soon as nothing else holds them, without a restart.
func TestSupersedingAWaitingRunReleasesItsReservationWithoutDrain(t *testing.T) {
	fixture := newGroupFixture(t, 0)
	lift := fixture.pausePreparation(t)
	head := fixture.branch(t, fixture.terraform, "feat-kv", "b")
	other := fixture.branch(t, fixture.exampleService, "small", "d")
	fixture.start(t)
	fixture.awaitPreparing(t, head)
	newer := fixture.branch(t, fixture.terraform, "feat-kv", "e")
	superseded := fixture.awaitTerminal(t, head.ID)
	if superseded.Status != model.RunInterrupted || superseded.SupersededBy != newer.ID {
		t.Fatalf("superseded head = status %s superseded_by %q", superseded.Status, superseded.SupersededBy)
	}
	if snapshot := fixture.scheduler.ownerForTest(head.QueueSequence); snapshot != nil {
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
