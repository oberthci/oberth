package service

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const kubevirtGroup = "kubevirt"

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

// The live tuxbox defect: an XL run of a concurrency group waits at the head
// of the FIFO admission queue while a smaller run holds weight. A newer push
// supersedes it, and the durable record turns interrupted, but its in-memory
// reservation stayed prepared at the head of the queue. Every later run, of the
// same group and of other repositories, kept waiting behind a run that no
// longer existed until the server restarted. Superseding it must withdraw the
// reservation at once, so the next grant pass admits the runs it blocked.
func TestSupersedingAWaitingXLHeadReleasesItsReservationOnTheNextGrantPass(t *testing.T) {
	fixture := newGroupFixture(t, 10)
	fixture.engine.declare("busy", "", periapsis.L)
	fixture.engine.declare("feat-kv", kubevirtGroup, periapsis.XL)
	fixture.engine.declare("feat-kv-2", kubevirtGroup, periapsis.S)
	fixture.engine.declare("small", "", periapsis.S)
	holder := fixture.branch(t, fixture.beacon, "busy", "a")
	head := fixture.branch(t, fixture.terraform, "feat-kv", "b")
	member := fixture.branch(t, fixture.terraform, "feat-kv-2", "c")
	small := fixture.branch(t, fixture.beacon, "small", "d")
	fixture.start(t)

	if created := fixture.awaitCreated(t, 1); !created[holder.ID] {
		t.Fatalf("created %v; want only the L holder", created)
	}
	fixture.awaitAdmission(t, head, "waiting XL at the queue head", func(snapshot *model.AdmissionObservation) bool {
		return snapshot != nil && snapshot.State == "waiting" && snapshot.Size == string(periapsis.XL) && snapshot.Position == 1 &&
			snapshot.Used == 3 && snapshot.Capacity == 20
	})
	fixture.awaitAdmission(t, member, "waiting behind the XL head in its group", func(snapshot *model.AdmissionObservation) bool {
		return snapshot != nil && snapshot.State == "waiting" && snapshot.GroupAhead == head.ID
	})
	fixture.awaitAdmission(t, small, "waiting behind the XL head", func(snapshot *model.AdmissionObservation) bool {
		return snapshot != nil && snapshot.State == "waiting"
	})
	fixture.assertNothingCreated(t)

	newer := fixture.branch(t, fixture.terraform, "feat-kv", "e")
	superseded := fixture.awaitTerminal(t, head.ID)
	if superseded.Status != model.RunInterrupted || superseded.SupersededBy != newer.ID {
		t.Fatalf("superseded head = status %s superseded_by %q", superseded.Status, superseded.SupersededBy)
	}
	created := fixture.awaitCreated(t, 2)
	if !created[member.ID] || !created[small.ID] {
		t.Fatalf("after the supersede the grant pass admitted %v; want the group member %s and the small run %s", created, member.ID, small.ID)
	}
	if snapshot := fixture.scheduler.AdmissionSnapshot(head.QueueSequence); snapshot != nil {
		t.Fatalf("superseded head still holds a reservation: %#v", snapshot)
	}
	// The replacement XL waits for the node and its group like any other run;
	// nothing names the superseded run any more.
	fixture.awaitAdmission(t, newer, "waiting replacement", func(snapshot *model.AdmissionObservation) bool {
		return snapshot != nil && snapshot.State == "waiting" && snapshot.GroupHolder == member.ID
	})
	fixture.assertNothingCreated(t)

	fixture.engine.finish(t, holder.ID)
	fixture.engine.finish(t, small.ID)
	fixture.engine.finish(t, member.ID)
	if next := fixture.awaitCreated(t, 1); !next[newer.ID] {
		t.Fatalf("after the node drained the gate admitted %v; want the replacement XL %s", next, newer.ID)
	}
	fixture.engine.finish(t, newer.ID)
	fixture.awaitTerminal(t, newer.ID)
	fixture.engine.assertNoViolations(t)
	if pending, err := fixture.store.PendingRunCancellations(t.Context()); err != nil || len(pending) != 0 {
		t.Fatalf("pending cancellations after the supersede = %#v, %v", pending, err)
	}
}
