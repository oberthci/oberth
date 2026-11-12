package service

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func probeGrant(g *admissionGate) { g.mu.Lock(); g.grantLocked(); g.mu.Unlock() }

// Whole-pool head (unknown graph fallback) under continuous small arrivals.
func TestReviewProbeWholePoolHeadStarves(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := newAdmissionGate()
	g.cpuMilli = 9600
	g.now = func() time.Time { return now }
	seq := int64(1)
	type live struct {
		r   *admissionReservation
		end time.Time
	}
	var running []live
	// Two smalls already running.
	for i := 0; i < 3; i++ {
		r := g.reservePriorityRun(seq, seq, model.Run{RepoID: 100 + seq, Priority: model.PriorityStandard})
		seq++
		r.prepared = true
		r.requests = &argoworkflow.PodRequests{CPUMilli: 2000}
		probeGrant(g)
		if !r.granted {
			t.Fatal("setup")
		}
		running = append(running, live{r, now.Add(time.Duration(4+i) * time.Minute)})
	}
	head := g.reservePriorityRun(seq, seq, model.Run{RepoID: 1, Priority: model.PriorityStandard})
	seq++
	head.prepared = true
	head.requests = clampAdmissionRequests(nil, 9600)
	probeGrant(g)
	var pending []*admissionReservation
	for tick := 0; tick < 4*60*4; tick++ { // 4 hours at 15s
		now = now.Add(15 * time.Second)
		kept := running[:0]
		for _, l := range running {
			if !now.Before(l.end) {
				l.r.release()
			} else {
				kept = append(kept, l)
			}
		}
		running = kept
		if tick%4 == 0 && len(g.waiters) < 8 { // one new small per minute, 4 minutes long
			r := g.reservePriorityRun(seq, seq, model.Run{RepoID: 100 + seq, Priority: model.PriorityStandard})
			seq++
			r.prepared = true
			r.requests = &argoworkflow.PodRequests{CPUMilli: 2000}
			pending = append(pending, r)
		}
		probeGrant(g)
		if head.granted {
			t.Logf("head granted after %s", time.Duration(tick+1)*15*time.Second)
			return
		}
		still := pending[:0]
		for _, r := range pending {
			if r.granted {
				running = append(running, live{r, now.Add(4 * time.Minute)})
			} else {
				still = append(still, r)
			}
		}
		pending = still
	}
	t.Fatalf("whole-pool head never granted in 4h; %d running, %d waiting", len(running), len(g.waiters))
}

// Two capacity-blocked waiters: does a fitting small ever backfill?
func TestReviewProbeTwoBlockedWaitersNoBackfill(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := newAdmissionGate()
	g.cpuMilli = 5000
	g.now = func() time.Time { return now }
	holder := g.reserve(1)
	holder.prepared = true
	holder.requests = &argoworkflow.PodRequests{CPUMilli: 3000}
	probeGrant(g)
	w1 := g.reserve(2)
	w1.prepared = true
	w1.requests = &argoworkflow.PodRequests{CPUMilli: 4000}
	w2 := g.reserve(3)
	w2.prepared = true
	w2.requests = &argoworkflow.PodRequests{CPUMilli: 4000}
	small := g.reserve(4)
	small.prepared = true
	small.requests = &argoworkflow.PodRequests{CPUMilli: 1000}
	probeGrant(g)
	for tick := 0; tick < 4*60; tick++ { // 60 minutes at 15 s
		now = now.Add(15 * time.Second)
		probeGrant(g)
		if small.granted {
			t.Logf("small backfilled after %s", time.Duration(tick+1)*15*time.Second)
			return
		}
	}
	t.Fatalf("small never backfilled in 60m behind two blocked waiters (2000m free)")
}

// Assisted hold: after one minute its whole reservation is given away.
func TestReviewProbeHeldLeaseOvercommit(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := newAdmissionGate()
	g.cpuMilli = 9600
	g.now = func() time.Time { return now }
	held := g.reserve(1)
	held.prepared = true
	held.requests = &argoworkflow.PodRequests{CPUMilli: 9600}
	probeGrant(g)
	held.heldSince = now
	now = now.Add(61 * time.Second)
	var total int64 = 9600
	for i := int64(2); i < 10; i++ {
		r := g.reserve(i)
		r.prepared = true
		r.requests = &argoworkflow.PodRequests{CPUMilli: 3000}
		probeGrant(g)
		if r.granted {
			total += 3000
		}
	}
	if total > 9600 {
		t.Fatalf("held live requests overcommitted: %dm", total)
	}
}
