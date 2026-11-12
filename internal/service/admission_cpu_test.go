package service

import (
	"context"
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
	now := time.Unix(100000, 0)
	g.now = func() time.Time { return now }
	completed := 0
	ends := map[int64]int{}
	for tick := 0; tick < 500 && completed < 50; tick++ {
		now = now.Add(15 * time.Second)
		var finished []*admissionReservation
		for seq, end := range ends {
			if end <= tick {
				finished = append(finished, g.reservations[seq])
				delete(ends, seq)
			}
		}
		for _, r := range finished {
			r.release()
			completed++
		}
		g.mu.Lock()
		g.grantLocked()
		var cpu int64
		for seq, r := range g.reservations {
			if r.granted && !r.released {
				cpu += g.credit(r, g.pool(r))
				if _, ok := ends[seq]; !ok {
					ends[seq] = tick + 2 + int(seq%8)
				}
			}
		}
		g.mu.Unlock()
		if cpu > 9600 {
			t.Fatalf("live simulated requests exceed pool: %dm", cpu)
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
func TestAdmissionCPUBoundedBackfillProtectsLargeWaiter(t *testing.T) {
	now := time.Unix(10000, 0)
	g := newAdmissionGate()
	g.cpuMilli = 5000
	g.now = func() time.Time { return now }
	holder := reviewPrepared(g, 1, 3000, model.PriorityStandard)
	large := reviewPrepared(g, 2, 4000, model.PriorityStandard)
	small := reviewPrepared(g, 3, 1000, model.PriorityStandard)
	if large.granted || !small.granted {
		t.Fatal("bounded backfill did not start")
	}
	snapshot := (&Scheduler{admission: g}).AdmissionSnapshot(2)
	if snapshot.BlockedBy == nil || !snapshot.BlockedBy.CPUCapacity {
		t.Fatal("CPU hold is not observable")
	}
	small.release()
	now = now.Add(cpuBackfillWindow)
	next := reviewPrepared(g, 4, 1000, model.PriorityStandard)
	if next.granted {
		t.Fatal("protected drain reopened")
	}
	holder.release()
	if !large.granted || !next.granted {
		t.Fatal("protected head or fitting successor did not start")
	}
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
		if err != nil || !r.granted || g.credit(r, g.pool(r)) != 9600 {
			t.Fatalf("reservation=%+v err=%v", r, err)
		}
		release()
	}
}
func TestAdmissionCPUAgedBackfillAndIndependentPools(t *testing.T) {
	now := time.Unix(10000, 0)
	g := newAdmissionGate()
	g.cpuMilli = 5000
	g.now = func() time.Time { return now }
	holder := reviewPrepared(g, 1, 3000, model.PriorityStandard)
	large := reviewPrepared(g, 2, 4000, model.PriorityStandard)
	now = now.Add(cpuBackfillWindow)
	remote := g.reserve(3)
	remote.cpu = argoworkflow.CPUAdmission{Pool: "remote", Milli: 1000}
	remote.poolKnown = true
	remote.prepared = true
	remote.requests = &argoworkflow.PodRequests{CPUMilli: 1000}
	probeGrant(g)
	if !remote.granted || large.granted {
		t.Fatal("local drain blocked another pool")
	}
	holder.release()
	if !large.granted {
		t.Fatal("head did not start after drain")
	}
}

func TestAdmissionCPUUnknownAdoptionAndAssistedHoldRetainLiveCredits(t *testing.T) {
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
		if waiter.granted || holder.released {
			t.Fatal("age discarded live CPU credit")
		}
	}
}

type failedAdoptedSizer struct{ JobController }

func (failedAdoptedSizer) AdoptedPodDemand(context.Context, model.Run) (*argoworkflow.PodDemand, error) {
	return nil, errors.New("unavailable demand binding")
}
func TestAdoptedDemandErrorDegradesToUnknownReservation(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	started := time.Now().Add(-time.Hour)
	run := model.Run{ID: "restored", QueueSequence: 1, StartedAt: &started}
	r, err := g.adopt(run)
	if err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{jobs: failedAdoptedSizer{}, admission: g}
	s.sizeAdoptedReservation(t.Context(), run, r)
	if !r.unknownAdopted || g.credit(r, g.pool(r)) != 9600 {
		t.Fatalf("demand read failure did not degrade to whole pool: %+v", r)
	}
}

func TestHardPressureBypassIsOnePerDeferralWindow(t *testing.T) {
	now := time.Unix(100000, 0)
	g := newAdmissionGate()
	g.now = func() time.Time { return now }
	c := DefaultAdmissionPressureConfig()
	c.HardOnly = true
	c.Logf = func(string, ...any) {}
	c.Source = func() (model.PressureSample, error) {
		return model.PressureSample{CPU: model.PressureResource{SomeAvg10: 90}}, nil
	}
	g.pressure = newPressureCheck(c)
	first := reviewPrepared(g, 1, 1000, model.PriorityStandard)
	second := reviewPrepared(g, 2, 1000, model.PriorityStandard)
	now = now.Add(300 * time.Second)
	probeGrant(g)
	if !first.granted || second.granted {
		t.Fatal("deferral granted more than one")
	}
	first.release()
	third := reviewPrepared(g, 3, 1000, model.PriorityStandard)
	probeGrant(g)
	if second.granted || third.granted {
		t.Fatal("claim/release reset pressure window")
	}
	now = now.Add(300 * time.Second)
	probeGrant(g)
	if !second.granted || third.granted {
		t.Fatal("next pressure window did not admit exactly one")
	}
}
