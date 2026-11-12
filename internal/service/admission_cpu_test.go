package service

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// A deterministic 12-core node: 20% host reserve and 50 mixed runs whose
// simulated Pods can start immediately on every grant. A slot is retained
// through execution, so no grant can create Insufficient cpu in this load.
func TestAdmissionCPUMixedLoad(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	g.holdUpdating = true
	for i := int64(1); i <= 50; i++ {
		r := g.reservePriorityRun(i, i, model.Run{RepoID: (i % 3) + 1, Priority: model.PriorityStandard})
		r.prepared = true
		cpu := int64(1000)
		if i%2 == 0 {
			cpu = 4000
		}
		r.requests = &argoworkflow.PodRequests{CPUMilli: cpu}
	}
	g.holdUpdating = false
	completed := 0
	for tick := 0; tick < 100 && completed < 50; tick++ {
		g.mu.Lock()
		g.grantLocked()
		var active []*admissionReservation
		var cpu int64
		for _, r := range g.reservations {
			if r.granted && !r.released {
				active = append(active, r)
				cpu += r.requests.CPUMilli
			}
		}
		if cpu > 9600 {
			t.Fatalf("started Pods exceed allocatable minus headroom: %dm", cpu)
		}
		if len(active) == 0 {
			t.Fatal("workload stopped making progress")
		}
		g.mu.Unlock()
		for _, r := range active {
			r.release()
			completed++
		}
	}
	if completed != 50 {
		t.Fatalf("completed %d runs", completed)
	}
}
func TestAdmissionCPUPromotionOvertakesAndRepositoryFairness(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 4000
	g.holdUpdating = true
	makeRun := func(seq, repo int64, p model.RunPriority, cpu int64) *admissionReservation {
		r := g.reservePriorityRun(seq, seq, model.Run{RepoID: repo, Priority: p})
		r.prepared = true
		r.requests = &argoworkflow.PodRequests{CPUMilli: cpu}
		return r
	}
	first := makeRun(1, 1, model.PriorityStandard, 4000)
	second := makeRun(2, 1, model.PriorityStandard, 4000)
	fair := makeRun(3, 2, model.PriorityStandard, 4000)
	promotion := makeRun(4, 3, model.PriorityPromotion, 4000)
	g.holdUpdating = false
	g.mu.Lock()
	g.grantLocked()
	g.mu.Unlock()
	if !promotion.granted || first.granted || fair.granted {
		t.Fatal("promotion did not overtake standards")
	}
	promotion.release()
	if !first.granted {
		t.Fatal("oldest unserved repository did not start")
	}
	first.release()
	if !fair.granted || second.granted {
		t.Fatal("busy repository defeated fair share")
	}
	fair.release()
	if !second.granted {
		t.Fatal("waiting repository stranded")
	}
	second.release()
}
func TestAdmissionCPUDoesNotBackfillPastLargeWaiter(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 5000
	holder := g.reserve(1)
	holder.requests = &argoworkflow.PodRequests{CPUMilli: 3000}
	holder.prepared = true
	g.mu.Lock()
	g.grantLocked()
	g.mu.Unlock()
	large := g.reserve(2)
	large.requests = &argoworkflow.PodRequests{CPUMilli: 4000}
	large.prepared = true
	small := g.reserve(3)
	small.requests = &argoworkflow.PodRequests{CPUMilli: 1000}
	small.prepared = true
	g.mu.Lock()
	g.grantLocked()
	g.mu.Unlock()
	if large.granted || small.granted {
		t.Fatal("small jobs must not keep a whole-run request from fitting")
	}
	snapshot := (&Scheduler{admission: g}).AdmissionSnapshot(2)
	if snapshot.BlockedBy == nil || !snapshot.BlockedBy.CPUCapacity {
		t.Fatal("CPU hold is not observable", snapshot)
	}
	holder.release()
	if !large.granted || !small.granted {
		t.Fatal("whole requests did not fit on release")
	}
	large.release()
	small.release()
}
func TestHardPressureDefaultDeferralIsBounded(t *testing.T) {
	now := time.Now()
	g := newAdmissionGate()
	g.now = func() time.Time { return now }
	c := DefaultAdmissionPressureConfig()
	c.HardOnly = true
	c.Source = func() (model.PressureSample, error) {
		return model.PressureSample{CPU: model.PressureResource{SomeAvg10: 90}}, nil
	}
	c.Logf = func(string, ...any) {}
	g.pressure = newPressureCheck(c)
	r := g.reserve(1)
	r.prepared = true
	g.mu.Lock()
	g.grantLocked()
	g.mu.Unlock()
	if r.granted {
		t.Fatal("high pressure did not hold")
	}
	now = now.Add(300 * time.Second)
	g.mu.Lock()
	g.grantLocked()
	g.mu.Unlock()
	if !r.granted {
		t.Fatal("pressure wait was unbounded")
	}
	r.release()
}

func TestAdmissionCPUDoesNotPreparePastDiskFloor(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	blockOnDiskFloor(g)
	r := g.reserve(1)
	r.prepared = true
	r.requests = &argoworkflow.PodRequests{CPUMilli: 1000}
	g.mu.Lock()
	g.grantLocked()
	g.mu.Unlock()
	if r.granted || g.spareSlotForClaim() {
		t.Fatal("CPU lookahead bypassed the checkout disk floor")
	}
	r.cancel()
}

func TestAdmissionCPUClampsUnknownAndWideFanout(t *testing.T) {
	for _, requests := range []*argoworkflow.PodRequests{nil, {CPUMilli: 10500}} {
		g := newAdmissionGate()
		g.cpuMilli = 9600
		r := g.reserve(1)
		release, err := r.acquireRequest(t.Context(), admissionRequest{Requests: requests, CPU: argoworkflow.CPUAdmission{Milli: 9600}})
		if err != nil || !r.granted || r.requests.CPUMilli != 9600 {
			t.Fatalf("reservation=%+v err=%v", r, err)
		}
		release()
	}
}
func TestAdmissionCPUAgedBackfillAndIndependentPools(t *testing.T) {
	now := time.Now()
	g := newAdmissionGate()
	g.cpuMilli = 5000
	g.now = func() time.Time { return now }
	holder := g.reserve(1)
	holder.prepared = true
	holder.requests = &argoworkflow.PodRequests{CPUMilli: 3000}
	g.mu.Lock()
	g.grantLocked()
	g.mu.Unlock()
	large := g.reserve(2)
	large.prepared = true
	large.requests = &argoworkflow.PodRequests{CPUMilli: 4000}
	small := g.reserve(3)
	small.prepared = true
	small.requests = &argoworkflow.PodRequests{CPUMilli: 1000}
	remote := g.reserve(4)
	remote.prepared = true
	remote.poolKnown = true
	remote.cpu = argoworkflow.CPUAdmission{Pool: "remote", Milli: 1000}
	remote.requests = &argoworkflow.PodRequests{CPUMilli: 1000}
	g.mu.Lock()
	g.grantLocked()
	g.mu.Unlock()
	if large.granted || small.granted || !remote.granted {
		t.Fatal("drain window or independent cluster violated")
	}
	now = now.Add(time.Minute)
	g.mu.Lock()
	g.grantLocked()
	g.mu.Unlock()
	if !small.granted || large.granted {
		t.Fatal("aged head prevented backfill")
	}
	small.release()
	now = now.Add(time.Minute)
	next := g.reserve(5)
	next.prepared = true
	next.requests = &argoworkflow.PodRequests{CPUMilli: 1000}
	g.mu.Lock()
	g.grantLocked()
	g.mu.Unlock()
	if next.granted {
		t.Fatal("continuous backfill defeated drain window")
	}
	holder.release()
	if !large.granted {
		t.Fatal("large run starved")
	}
}
func TestAdmissionCPUUnknownAdoptionAndAssistedHoldHaveBoundedLeases(t *testing.T) {
	for _, assisted := range []bool{false, true} {
		now := time.Now()
		g := newAdmissionGate()
		g.cpuMilli = 5000
		g.now = func() time.Time { return now }
		holder, err := g.adopt(model.Run{ID: "adopted", QueueSequence: 1, StartedAt: &now})
		if err != nil {
			t.Fatal(err)
		}
		if assisted {
			holder.unknownAdopted = false
			holder.requests = &argoworkflow.PodRequests{CPUMilli: 5000}
			holder.heldSince = now
		}
		waiter := g.reserve(2)
		waiter.prepared = true
		waiter.requests = &argoworkflow.PodRequests{CPUMilli: 5000}
		g.mu.Lock()
		g.grantLocked()
		g.mu.Unlock()
		if waiter.granted {
			t.Fatal("lease skipped")
		}
		now = now.Add(time.Minute)
		g.mu.Lock()
		g.grantLocked()
		g.mu.Unlock()
		if !waiter.granted || holder.released {
			t.Fatal("expired lease blocked or discarded resource ownership")
		}
	}
}

type failedAdoptedSizer struct{ JobController }

func (failedAdoptedSizer) AdoptedPodDemand(model.Run) (*argoworkflow.PodDemand, error) {
	return nil, errors.New("unavailable demand binding")
}
func TestAdoptedDemandErrorDegradesToUnknownReservation(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	run := model.Run{ID: "restored", QueueSequence: 1}
	r, err := g.adopt(run)
	if err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{jobs: failedAdoptedSizer{}, admission: g}
	s.sizeAdoptedReservation(t.Context(), run, r)
	if !r.unknownAdopted || r.requests.CPUMilli != 9600 {
		t.Fatalf("demand read failure did not degrade to whole pool: %+v", r)
	}
}
