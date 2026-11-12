package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type reviewR2Pools struct {
	JobController
	pools []argoworkflow.CPUAdmission
}

func (j *reviewR2Pools) AdmissionCPUPools(context.Context) []argoworkflow.CPUAdmission {
	return j.pools
}
func TestReviewR2PinnedPoolMustNotHoldHealthyLocal(t *testing.T) {
	g := newAdmissionGate()
	g.cpuPool = "https://same-api"
	g.cpuMilli = 9600
	local := argoworkflow.CPUAdmission{Pool: g.cpuPool, Milli: 9600, NodeMilli: 12000, StepMaxMilli: 3000}
	pinned := argoworkflow.CPUAdmission{Pool: g.cpuPool, HoldReason: "CPU admission held: selected node unavailable"}
	s := &Scheduler{admission: g, jobs: &reviewR2Pools{pools: []argoworkflow.CPUAdmission{local, pinned}}}
	s.refreshAdmission(t.Context())
	r := g.reserve(1)
	r.cpu = local
	r.poolKnown = true
	r.prepared = true
	r.requests = &argoworkflow.PodRequests{CPUMilli: 1000}
	probeGrant(g)
	if !r.granted {
		t.Fatalf("healthy 9600m local pool was replaced by pinned target snapshot: %+v", g.pool(r))
	}
}
func TestReviewR2LiveGrowthCharacterization(t *testing.T) {
	now := time.Unix(10000, 0)
	g := newAdmissionGate()
	g.cpuMilli = 9600
	g.now = func() time.Time { return now }
	r := reviewPrepared(g, 1, 1500, model.PriorityHigh)
	r.requests.CPUMilli = 9600
	r.heldSince = now
	r.trackLive = true
	r.liveRequests = &argoworkflow.PodRequests{CPUMilli: 1500}
	tail := reviewPrepared(g, 2, 8000, model.PriorityHigh)
	if !tail.granted {
		t.Fatal("setup did not admit backfill")
	}
	now = now.Add(15 * time.Second)
	r.liveRequests = &argoworkflow.PodRequests{CPUMilli: 6000}
	r.heldSince = time.Time{}
	probeGrant(g)
	total := g.credit(r, g.pool(r)) + g.credit(tail, g.pool(tail))
	t.Logf("after next resident observation: granted credit=%dm pool=%dm", total, g.cpuMilli)
	if total != 14000 {
		t.Fatal("characterization changed", total)
	}
	tail2 := reviewPrepared(g, 3, 500, model.PriorityStandard)
	if tail2.granted || r.released || tail.released {
		t.Fatal("live growth discarded existing grants or admitted more CPU")
	}
}

type reviewR2SlowReads struct {
	liveCPUFixture
	now       *time.Time
	scheduler *Scheduler
	stalled   bool
	calls     int
}

func (j *reviewR2SlowReads) LivePodRequests(context.Context, model.Run) (*argoworkflow.PodRequests, error) {
	*j.now = j.now.Add(2 * time.Second)
	j.calls++
	if j.scheduler.Live() != nil {
		j.stalled = true
	}
	return nil, context.DeadlineExceeded
}
func TestReviewR2ExternalRefreshMustNotAgeLiveness(t *testing.T) {
	f := newGroupFixture(t, 0)
	s := f.scheduler
	g := s.admission
	g.cpuMilli = 9600
	now := time.Unix(10000, 0)
	g.now = func() time.Time { return now }
	s.dispatchProgress.Now = g.now
	// Regression: 251 two-second reads must stay off the dispatch path.
	j := &reviewR2SlowReads{liveCPUFixture: liveCPUFixture{pool: argoworkflow.CPUAdmission{Milli: 9600}}, now: &now, scheduler: s}
	s.jobs = j
	for seq := int64(1000); seq < 1251; seq++ {
		old := f.branch(t, f.exampleService, fmt.Sprintf("old-%d", seq), "a")
		r, err := g.adopt(old)
		if err != nil {
			t.Fatal(err)
		}
		r.poolKnown = true
		r.unknownAdopted = true
	}
	s.dispatchProgress.Beat()
	s.refreshAdmission(withDispatchProgress(t.Context(), &s.dispatchProgress))
	if j.calls != 0 {
		t.Fatal("dispatch made external CPU reads", j.calls)
	}
	if j.stalled {
		t.Fatalf("%d bounded external reads made healthy dispatch fail liveness after %s", j.calls, now.Sub(time.Unix(10000, 0)))
	}
}
