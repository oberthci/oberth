package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type liveCPUFixture struct {
	JobController
	pool             argoworkflow.CPUAdmission
	poolErr, liveErr error
	demand           *argoworkflow.PodDemand
	live             *argoworkflow.PodRequests
	holds            []model.AssistHold
}

func (j *liveCPUFixture) AdoptedPodDemand(context.Context, model.Run) (*argoworkflow.PodDemand, error) {
	return j.demand, nil
}
func (j *liveCPUFixture) AdoptedAdmissionCPU(context.Context, model.Run) (argoworkflow.CPUAdmission, error) {
	return j.pool, j.poolErr
}
func (j *liveCPUFixture) AdmissionCPUPools(context.Context) []argoworkflow.CPUAdmission {
	return []argoworkflow.CPUAdmission{j.pool}
}
func (j *liveCPUFixture) LivePodRequests(context.Context, model.Run) (*argoworkflow.PodRequests, error) {
	return j.live.Clone(), j.liveErr
}
func (j *liveCPUFixture) ListAssistHolds(context.Context, string) ([]model.AssistHold, error) {
	return j.holds, nil
}

func TestUnknownUpgradeCPUReadFailureRetainsPoolAndRetries(t *testing.T) {
	f := newGroupFixture(t, 0)
	g := f.scheduler.admission
	g.cpuMilli = 9600
	g.cpuPool = "cluster"
	old := f.branch(t, f.exampleService, "old", "a")
	started := time.Now().Add(-time.Hour)
	old.StartedAt = &started
	j := &liveCPUFixture{JobController: f.engine, pool: argoworkflow.CPUAdmission{Pool: "cluster", Milli: 9600}, liveErr: errors.New("pod read denied")}
	f.scheduler.jobs = j
	holder, err := g.adopt(old)
	if err != nil {
		t.Fatal(err)
	}
	f.scheduler.sizeAdoptedReservation(t.Context(), old, holder)
	if g.credit(holder, g.pool(holder)) != 9600 {
		t.Fatal("upgrade gave away unknown CPU")
	}
	waiter := reviewPrepared(g, old.QueueSequence+1, 9000, model.PriorityStandard)
	if waiter.granted {
		t.Fatal("unknown resident requests were ignored")
	}
	j.liveErr = nil
	j.live = &argoworkflow.PodRequests{CPUMilli: 1500, Pods: 2}
	f.scheduler.refreshReservationCPU(t.Context(), holder)
	if snapshot := f.scheduler.AdmissionSnapshot(holder.sequence); snapshot.Requests.CPUMilli != 1500 {
		t.Fatal("displayed CPU differs from live credit", snapshot)
	}
	if g.credit(holder, g.pool(holder)) != 1500 {
		t.Fatal("live read did not replace whole-pool fallback")
	}
	waiter.requests.CPUMilli = 8000
	probeGrant(g)
	if !waiter.granted {
		t.Fatal("observed free capacity did not admit fitting work")
	}
	if total := g.credit(holder, g.pool(holder)) + g.credit(waiter, g.pool(waiter)); total > 9600 {
		t.Fatal("live CPU overcommit", total)
	}
}
func TestAssistedContinuationRetainsResidentAndSiblingRequests(t *testing.T) {
	f := newGroupFixture(t, 0)
	g := f.scheduler.admission
	g.cpuMilli = 9600
	queued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "assist", Repository: f.exampleService, Branch: "held", SHA: strings.Repeat("a", 40), Actor: "agent", Assist: true})
	if err != nil {
		t.Fatal(err)
	}
	holder, err := g.adopt(queued.Run)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Hour)
	j := &liveCPUFixture{JobController: f.engine, pool: argoworkflow.CPUAdmission{Milli: 9600}, demand: &argoworkflow.PodDemand{CPUMilli: 9600, MemoryBytes: 1 << 30, Pods: 3}, live: &argoworkflow.PodRequests{CPUMilli: 6000, Pods: 3}, holds: []model.AssistHold{{Pod: "held", HeldSince: since}}}
	f.scheduler.jobs = j
	f.scheduler.sizeAdoptedReservation(t.Context(), queued.Run, holder)
	f.scheduler.refreshReservationCPU(t.Context(), holder)
	if g.credit(holder, g.pool(holder)) != 6000 {
		t.Fatal("held Pod or sibling CPU omitted")
	}
	next := reviewPrepared(g, queued.QueueSequence+1, 3000, model.PriorityStandard)
	if !next.granted {
		t.Fatal("unused peak capacity did not become available")
	}
	j.holds = nil
	f.scheduler.refreshReservationCPU(t.Context(), holder)
	if !holder.trackLive || g.credit(holder, g.pool(holder)) != 6000 {
		t.Fatal("continue reacquired or discarded resident credit")
	}
	if total := g.credit(holder, g.pool(holder)) + g.credit(next, g.pool(next)); total > 9600 {
		t.Fatal("continue overcommitted CPU", total)
	}
}
func TestCanonicalPoolSurvivesAdoptionErrorAndCreateRestoration(t *testing.T) {
	f := newGroupFixture(t, 0)
	g := f.scheduler.admission
	g.cpuMilli = 9600
	g.cpuPool = "cluster"
	run := f.branch(t, f.exampleService, "restore", "a")
	f.scheduler.jobs = &liveCPUFixture{JobController: f.engine, poolErr: errors.New("target lookup failed"), liveErr: errors.New("read denied")}
	restored, err := f.scheduler.restoreCreateGrant(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.pool(restored).Pool != "cluster" {
		t.Fatal("create grant fell into invisible pool")
	}
	f.scheduler.sizeAdoptedReservation(t.Context(), run, restored)
	waiter := g.reserve(2)
	waiter.poolKnown = true
	waiter.cpu = argoworkflow.CPUAdmission{Pool: "cluster", Milli: 9600}
	waiter.requests = &argoworkflow.PodRequests{CPUMilli: 9000}
	waiter.prepared = true
	probeGrant(g)
	if waiter.granted || g.pool(restored).Pool != "cluster" {
		t.Fatal("failed pool lookup discarded restored CPU")
	}
}
func TestCapacityHeldPoolRefreshUnblocksWithoutRestart(t *testing.T) {
	g := newAdmissionGate()
	g.cpuPool = "cluster"
	j := &liveCPUFixture{pool: argoworkflow.CPUAdmission{Pool: "cluster", HoldReason: "CPU admission held: node capacity unavailable"}}
	s := &Scheduler{admission: g, jobs: j}
	s.refreshAdmission(t.Context())
	waiter := g.reserve(1)
	waiter.cpu = j.pool
	waiter.poolKnown = true
	waiter.requests = &argoworkflow.PodRequests{CPUMilli: 1000}
	waiter.prepared = true
	probeGrant(g)
	if waiter.granted {
		t.Fatal("unknown capacity admitted work")
	}
	if reason := g.blockerLocked(waiter).Reason(); !strings.Contains(reason, "node capacity unavailable") {
		t.Fatal("capacity hold has no clear reason", reason)
	}
	j.pool = argoworkflow.CPUAdmission{Pool: "cluster", Milli: 9600}
	s.refreshAdmission(t.Context())
	if !waiter.granted {
		t.Fatal("capacity recovery still requires restart")
	}
}

func TestFullCPULookaheadStillClaimsPromotionAndHigh(t *testing.T) {
	for _, priority := range []model.RunPriority{model.PriorityPromotion, model.PriorityHigh} {
		t.Run(string(priority), func(t *testing.T) {
			f := newGroupFixture(t, 0)
			f.scheduler.admission.cpuMilli = 9600
			f.engine.declareCPU("holder", 7000)
			f.engine.declareCPU("urgent", 2000)
			holder := f.branch(t, f.exampleService, "holder", "a")
			var tail model.Run
			for i := range 8 {
				ref := fmt.Sprintf("standard-%d", i)
				f.engine.declareCPU(ref, 3000)
				tail = f.branch(t, f.exampleService, ref, "b")
			}
			f.start(t)
			if created := f.awaitCreated(t, 1); !created[holder.ID] {
				t.Fatal("wrong initial run", created)
			}
			f.awaitAdmission(t, tail, "full prepared lookahead", func(o *model.AdmissionObservation) bool { return o != nil && o.State == "waiting" })
			urgent := f.branch(t, f.exampleService, "urgent", "c")
			if _, err := f.store.BumpRunPriority(t.Context(), "admin", urgent.ID, priority, "urgent work"); err != nil {
				t.Fatal(err)
			}
			f.scheduler.NotifyQueue()
			if created := f.awaitCreated(t, 1); !created[urgent.ID] {
				t.Fatal("full standard lookahead hid urgent work", created)
			}
			f.engine.finish(t, urgent.ID)
			f.awaitTerminal(t, urgent.ID)
		})
	}
}

func TestUnpreparedCheckoutOnlyBlocksItsOwnCPUPool(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 5000
	remote := g.reserve(1)
	remote.cpu = argoworkflow.CPUAdmission{Pool: "remote", Milli: 5000}
	remote.poolKnown = true
	local := reviewPrepared(g, 2, 1000, model.PriorityStandard)
	if !local.granted || remote.granted {
		t.Fatal("remote checkout blocked local capacity")
	}
}
