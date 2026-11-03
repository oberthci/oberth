package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestStalledAdmissionDrainCap(t *testing.T) {
	for _, elapsed := range []time.Duration{24 * time.Minute, 25 * time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			g, clock, running := easyGate(t)
			g.mu.Lock()
			running.estimate = 0
			g.mu.Unlock()
			head := pending(t, g, 2, easyRequest("head", 8200, time.Hour))
			t.Cleanup(head.cancel)
			clock.now = clock.now.Add(elapsed)
			candidate := pending(t, g, 3, easyRequest("candidate", 4000, 0))
			t.Cleanup(candidate.cancel)
			if granted(g, candidate) != (elapsed >= 25*time.Minute) {
				t.Fatalf("drain cap did not restore spare-capacity backfill after %s", elapsed)
			}
			if elapsed >= 25*time.Minute {
				view := (&Scheduler{admission: g}).AdmissionSnapshot(candidate.sequence)
				if !strings.Contains(view.BackfillMessage, "drain limit") {
					t.Fatalf("missing existing-line reason: %+v", view)
				}
				if !granted(g, running) || granted(g, head) {
					t.Fatal("cap fabricated capacity or discarded a live reservation")
				}
			}
		})
	}
}

func TestStalledAdmissionDrainCapKeepsPressureAndStrictFIFO(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "pressure", true: "strict FIFO"}[strict], func(t *testing.T) {
			f := pressureTestGate()
			f.gate.maxHeadWait = 20 * time.Minute
			if strict {
				f.gate.maxHeadWait = 0
			}
			running := f.prepare(1, model.PriorityNormal, "", pipeline.M)
			t.Cleanup(running.release)
			head := f.prepare(2, model.PriorityNormal, "", pipeline.XL)
			t.Cleanup(head.cancel)
			f.sample.IO.FullAvg60 = 80
			f.advance(30 * time.Minute)
			candidate := f.prepare(3, model.PriorityNormal, "", pipeline.S)
			t.Cleanup(candidate.cancel)
			if candidate.granted {
				t.Fatal("drain cap bypassed pressure or strict FIFO")
			}
			if !strict && !candidate.pressureDeferred {
				t.Fatal("drain cap never reached the pressure gate")
			}
		})
	}
}

// Reproduce the live combination: two idle grants, an aged XL head and spare
// resources that must not remain unusable forever. All actual grants still
// count until the Workflow cleanup owner releases them.
func TestStalledAdmissionCombinedReservationsAndXLDrain(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 3, 18, 57, 0, 0, time.UTC)}
	g := backfillGate(clock)
	g.resources.CPUMilli, g.resources.MemoryBytes = 32000, 64<<30
	identity := g.reserve(1)
	acquireRequestWithinBudget(t, identity, easyRequest("identity-stalled", 18750, 0))
	t.Cleanup(identity.release)
	idle := g.reserve(2)
	acquireRequestWithinBudget(t, idle, easyRequest("waiting-stalled", 2450, 0))
	t.Cleanup(idle.release)
	head := pending(t, g, 3, admissionRequest{RunID: "xl-head", Size: pipeline.XL})
	t.Cleanup(head.cancel)
	clock.now = clock.now.Add(25 * time.Minute)
	small := pending(t, g, 4, easyRequest("backfill", 1100, 0))
	t.Cleanup(small.release)
	if !granted(g, small) || granted(g, head) {
		t.Fatal("aged XL drain blocked spare-capacity work")
	}
	tooLarge := pending(t, g, 5, easyRequest("over-capacity", 12000, 0))
	t.Cleanup(tooLarge.cancel)
	if granted(g, tooLarge) {
		t.Fatal("drain escape bypassed real resource capacity")
	}
	tooLarge.cancel()
	identity.release()
	idle.release()
	if granted(g, head) {
		t.Fatal("XL started beside the remaining backfill")
	}
	small.release()
	if !granted(g, head) {
		t.Fatal("head did not start after actual cleanup released its blockers")
	}
}
