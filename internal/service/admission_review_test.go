package service

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func reviewPrepared(g *admissionGate, seq, cpu int64, priority model.RunPriority) *admissionReservation {
	r := g.reservePriorityRun(seq, seq, model.Run{RepoID: 1, Priority: priority})
	g.mu.Lock()
	r.prepared = true
	r.requests = &argoworkflow.PodRequests{CPUMilli: cpu}
	g.grantLocked()
	g.mu.Unlock()
	return r
}

func TestReviewBackfillMustEventuallyDrain(t *testing.T) {
	start := time.Unix(10000, 0)
	now := start
	g := newAdmissionGate()
	g.cpuMilli = 5000
	g.now = func() time.Time { return now }
	type running struct {
		r   *admissionReservation
		end int
	}
	active := []running{{reviewPrepared(g, 1, 1000, model.PriorityHigh), 130}}
	large := reviewPrepared(g, 2, 5000, model.PriorityHigh)
	var seq int64 = 3
	completed := 0
	for second := 1; second <= 1200; second++ {
		now = start.Add(time.Duration(second) * time.Second)
		for i := 0; i < len(active); {
			if active[i].end <= second {
				active[i].r.release()
				completed++
				active = append(active[:i], active[i+1:]...)
			} else {
				i++
			}
		}
		if second%120 == 60 {
			r := reviewPrepared(g, seq, 1000, model.PriorityHigh)
			seq++
			if r.granted {
				active = append(active, running{r, second + 130})
			}
		}
		g.mu.Lock()
		g.grantLocked()
		g.mu.Unlock()
		if large.granted {
			return
		}
	}
	t.Fatalf("large 5000m run still waiting after 20 minutes and %d completed 130-second small runs; each backfill window replaced the blocker before it drained", completed)
}

func TestReviewLeaseMustNotDiscardLiveCPU(t *testing.T) {
	for _, mode := range []string{"unknown-adopted", "assisted-held", "assisted-resumed"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(10000, 0)
			started := now.Add(-2 * time.Minute)
			g := newAdmissionGate()
			g.cpuMilli = 9600
			g.now = func() time.Time { return now }
			holder, err := g.adopt(model.Run{ID: "existing", QueueSequence: 1, StartedAt: &started})
			if err != nil {
				t.Fatal(err)
			}
			holder.requests = &argoworkflow.PodRequests{CPUMilli: 9600}
			if mode != "unknown-adopted" {
				holder.unknownAdopted = false
				holder.heldSince = started
			}
			waiter := reviewPrepared(g, 2, 9600, model.PriorityStandard)
			if mode == "assisted-resumed" {
				holder.heldSince = time.Time{}
				g.mu.Lock()
				g.grantLocked()
				g.mu.Unlock()
			}
			var live int64
			for _, r := range g.reservations {
				if r.granted && !r.released {
					live += r.requests.CPUMilli
				}
			}
			if live > g.cpuMilli {
				t.Fatalf("active reservations total %dm > pool %dm, oldReleased=%v newGranted=%v", live, g.cpuMilli, holder.released, waiter.granted)
			}
		})
	}
}

type reviewPriorityReader struct {
	SchedulerStore
	next model.Run
}

func (s *reviewPriorityReader) NextQueuedRun(context.Context) (model.Run, error) { return s.next, nil }

func TestReviewFullLookaheadMustAllowHigherPriority(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	reviewPrepared(g, 1, 9600, model.PriorityStandard)
	for seq := int64(2); seq <= 9; seq++ {
		reviewPrepared(g, seq, 9600, model.PriorityStandard)
	}
	s := &Scheduler{admission: g, store: &reviewPriorityReader{next: model.Run{ID: "urgent", QueueSequence: 10, AdmissionSequence: 10, Priority: model.PriorityHigh}}}
	if s.dispatchSlotAvailable(9) {
		t.Fatal("fixture did not fill lookahead")
	}
	available, err := s.priorityDispatchAvailable(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("urgent high-priority run cannot be claimed behind eight standard waiters")
	}
}
