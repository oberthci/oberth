package service

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// Short work may backfill every class; long work cannot consume capacity
// needed by the head when the first running grant finishes.
func TestPriorityReviewBackfillPolicyForAllClasses(t *testing.T) {
	for _, priority := range []model.RunPriority{model.PriorityNormal, model.PriorityHigh, model.PriorityHighest} {
		t.Run(string(priority), func(t *testing.T) {
			clock := &fakeClock{now: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)}
			g := newWeightedAdmission(6)
			g.now, g.maxHeadWait = clock.Now, 20*time.Minute
			long := g.reserve(1)
			acquireRequestWithinBudget(t, long, admissionRequest{Size: periapsis.L, Estimate: time.Hour})
			t.Cleanup(long.release)
			short := g.reserve(2)
			acquireRequestWithinBudget(t, short, admissionRequest{Size: periapsis.S, Estimate: 10 * time.Minute})
			t.Cleanup(short.release)
			head := priorityWaiter(g, 3, priority, clock.now, periapsis.L)
			t.Cleanup(head.cancel)
			delaying := priorityWaiter(g, 4, priority, clock.now, periapsis.S, 30*time.Minute)
			t.Cleanup(delaying.cancel)
			safe := priorityWaiter(g, 5, priority, clock.now, periapsis.S, time.Minute)
			t.Cleanup(safe.cancel)
			if granted(g, head) || granted(g, delaying) || !granted(g, safe) {
				t.Fatal("admission lost safe backfill or admitted a candidate that delays the head")
			}
		})
	}
}
