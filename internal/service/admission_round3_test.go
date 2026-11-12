package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/store"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// A held remote pool must not consume the local preparation lookahead.
func TestR2ProbeHeldRemotePoolBlocksLocalClaims(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	g.cpuPool = "local"
	g.pools = map[string]argoworkflow.CPUAdmission{
		"local":  {Pool: "local", Milli: 9600},
		"remote": {Pool: "remote", HoldReason: "CPU admission held: node capacity unavailable"},
	}
	for i := int64(1); i <= 8; i++ {
		r := g.reserve(i)
		g.mu.Lock()
		r.cpu, r.poolKnown, r.prepared = argoworkflow.CPUAdmission{Pool: "remote"}, true, true
		r.requests = &argoworkflow.PodRequests{CPUMilli: 500}
		g.grantLocked()
		g.mu.Unlock()
	}
	if g.spareSlotForClaim() {
		t.Log("local claim allowed")
		return
	}
	t.Fatal("8 waiters on a held remote pool stop all local claims although local pool is empty")
}

// Assisted hold keeps resident CPU; a protected head freezes the pool for the hold.
func TestR2ProbeHeldRunFreezesPoolBehindProtectedHead(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := newAdmissionGate()
	g.cpuMilli = 9600
	g.now = func() time.Time { return now }
	held := reviewPrepared(g, 1, 9600, model.PriorityStandard)
	g.mu.Lock()
	held.trackLive, held.heldSince = true, now
	held.liveRequests = &argoworkflow.PodRequests{CPUMilli: 3050, Pods: 1}
	g.mu.Unlock()
	head := reviewPrepared(g, 2, 7000, model.PriorityStandard)
	granted := 0
	var seq int64 = 3
	for tick := 0; tick < 4*40; tick++ { // 40 minutes
		now = now.Add(15 * time.Second)
		if tick%4 == 0 {
			r := g.reservePriorityRun(seq, seq, model.Run{RepoID: 50 + seq, Priority: model.PriorityStandard})
			seq++
			g.mu.Lock()
			r.prepared = true
			r.requests = &argoworkflow.PodRequests{CPUMilli: 500}
			g.mu.Unlock()
		}
		probeGrant(g)
		for _, r := range g.reservations {
			if r.granted && r.sequence > 2 && !r.released {
				granted++
				r.release()
			}
		}
	}
	if head.granted || granted != 40 {
		t.Fatalf("head granted=%v; small runs=%d, want 40", head.granted, granted)
	}
	// Ending the hold makes the head attainable without losing its drain clock.
	held.heldSince = time.Time{}
	held.liveRequests = nil
	held.trackLive = false
	held.release()
	if !head.granted {
		t.Fatal("head lost its queue place when the hold ended")
	}
}

func TestUnattainableHeadLeavesRetainedCreditsCharged(t *testing.T) {
	for _, mode := range []string{"cleanup", "failed-read"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(10000, 0)
			g := newAdmissionGate()
			g.cpuMilli = 9600
			g.now = func() time.Time { return now }
			holder := reviewPrepared(g, 1, 3050, model.PriorityStandard)
			if mode == "cleanup" {
				holder.retain()
			} else {
				holder.unknownAdopted = true
				holder.requests = nil
			}
			head := reviewPrepared(g, 2, 7000, model.PriorityStandard)
			now = now.Add(10 * time.Minute)
			if g.headCanDrainLocked(head) {
				t.Fatal("unreleasable CPU treated as finite work")
			}
			small := reviewPrepared(g, 3, 500, model.PriorityStandard)
			if mode == "cleanup" && !small.granted {
				t.Fatal("retained cleanup froze free CPU")
			}
			if mode == "failed-read" && small.granted {
				t.Fatal("failed read gave away resident CPU")
			}
			if holder.released {
				t.Fatal("CPU credit expired")
			}
		})
	}
}

func TestHardPressureBypassWindowsArePerPool(t *testing.T) {
	now := time.Unix(100000, 0)
	g := newAdmissionGate()
	g.cpuMilli = 9600
	g.now = func() time.Time { return now }
	c := DefaultAdmissionPressureConfig()
	c.HardOnly = true
	c.Logf = func(string, ...any) {}
	c.Source = func() (model.PressureSample, error) {
		return model.PressureSample{CPU: model.PressureResource{SomeAvg10: 90}}, nil
	}
	g.pressure = newPressureCheck(c)
	prepare := func(seq int64, pool string) *admissionReservation {
		r := g.reserve(seq)
		r.cpu = argoworkflow.CPUAdmission{Pool: pool, Milli: 9600}
		r.poolKnown = true
		r.requests = &argoworkflow.PodRequests{CPUMilli: 500}
		r.prepared = true
		probeGrant(g)
		return r
	}
	local, remote := prepare(1, "local"), prepare(2, "remote")
	now = now.Add(300 * time.Second)
	probeGrant(g)
	if !local.granted || !remote.granted {
		t.Fatal("one pool consumed another's pressure allowance")
	}
	nextLocal, nextRemote := prepare(3, "local"), prepare(4, "remote")
	local.release()
	remote.release()
	probeGrant(g)
	if nextLocal.granted || nextRemote.granted {
		t.Fatal("release renewed pool pressure allowance")
	}
	now = now.Add(300 * time.Second)
	probeGrant(g)
	if !nextLocal.granted || !nextRemote.granted {
		t.Fatal("next per-pool pressure window did not grant")
	}
}

type poolQueueJobs struct {
	JobController
	remoteRepo int64
}

func (j poolQueueJobs) AdoptedAdmissionCPU(_ context.Context, run model.Run) (argoworkflow.CPUAdmission, error) {
	pool := "local"
	if run.RepoID == j.remoteRepo {
		pool = "remote"
	}
	return argoworkflow.CPUAdmission{Pool: pool, Milli: 9600}, nil
}
func TestDispatcherSkipsFullPoolWithoutClaimingItsQueuedRuns(t *testing.T) {
	f := newGroupFixture(t, 0)
	s := f.scheduler
	g := s.admission
	g.cpuMilli = 9600
	g.cpuPool = "local"
	g.pools = map[string]argoworkflow.CPUAdmission{"remote": {Pool: "remote", HoldReason: "CPU admission held: node unavailable"}}
	s.jobs = poolQueueJobs{JobController: f.engine, remoteRepo: f.terraform.ID}
	for seq := int64(100); seq < 108; seq++ {
		r := g.reserve(seq)
		r.cpu = argoworkflow.CPUAdmission{Pool: "remote"}
		r.poolKnown = true
		r.prepared = true
		r.requests = &argoworkflow.PodRequests{CPUMilli: 500}
		probeGrant(g)
	}
	blocked := f.branch(t, f.terraform, "blocked", "a")
	local := f.branch(t, f.exampleService, "local", "b")
	claimed, err := s.claimDispatchRun(t.Context(), 8)
	if err != nil || claimed.ID != local.ID {
		t.Fatalf("local claim=%+v %v", claimed, err)
	}
	s.reserveAdmission(t.Context(), claimed) // local checkout is still preparing
	f.branch(t, f.exampleService, "next-local", "c")
	if _, err := s.claimDispatchRun(t.Context(), 9); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("more than one checkout per pool: %v", err)
	}
	current, err := f.store.Run(t.Context(), blocked.ID)
	if err != nil || current.Status != model.RunQueued {
		t.Fatal("full pool overclaimed", current, err)
	}
	urgent := f.branch(t, f.terraform, "urgent", "d")
	if _, err := f.store.BumpRunPriority(t.Context(), "admin", urgent.ID, model.PriorityHigh, "release"); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.claimDispatchRun(t.Context(), 9)
	if err != nil || claimed.ID != urgent.ID {
		t.Fatal("full pool hid higher priority work", claimed, err)
	}
}

func TestFailedRefreshCannotDiscardObservedExcessCPU(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	r := reviewPrepared(g, 1, 9600, model.PriorityStandard)
	r.trackLive = true
	r.liveRequests = &argoworkflow.PodRequests{CPUMilli: 14000, Pods: 4}
	s := &Scheduler{admission: g}
	s.clearLiveCPU(r)
	if !r.liveReadFailed || g.credit(r, g.pool(r)) != 14000 {
		t.Fatal("failed read discarded observed resident CPU")
	}
	head := reviewPrepared(g, 2, 7000, model.PriorityStandard)
	if head.granted || g.headCanDrainLocked(head) {
		t.Fatal("failed observation made the head attainable")
	}
}
