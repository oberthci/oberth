package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type r3ReviewDeadlineStore struct {
	SchedulerStore
	observed chan time.Duration
	block    bool
}

func (s *r3ReviewDeadlineStore) Run(ctx context.Context, id string) (model.Run, error) {
	deadline, _ := ctx.Deadline()
	s.observed <- time.Until(deadline)
	if s.block {
		<-ctx.Done()
		return model.Run{}, ctx.Err()
	}
	return model.Run{ID: id}, nil
}
func TestReviewR3RefreshHonorsProductionReadBudget(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	r, _ := g.adopt(model.Run{ID: "owned", QueueSequence: 1})
	r.poolKnown = true
	db := &r3ReviewDeadlineStore{observed: make(chan time.Duration, 4)}
	s := &Scheduler{admission: g, store: db, jobs: &liveCPUFixture{pool: argoworkflow.CPUAdmission{Milli: 9600}}}
	// Scheduler.Run installs this same scope before starting runAdmissionRefresh.
	ctx := context.WithValue(t.Context(), databaseRetryKey{}, &databaseRetryScope{lifetime: t.Context(), owner: s})
	s.refreshCPUReservations(ctx)
	if budget := <-db.observed; budget > 2500*time.Millisecond {
		t.Fatalf("two-second CPU read received a %s storage deadline under production retry scope", budget)
	}
}
func TestReviewR3RefreshCycleHonorsBudgetWithProductionRetry(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	r, _ := g.adopt(model.Run{ID: "owned", QueueSequence: 1})
	r.poolKnown = true
	db := &r3ReviewDeadlineStore{observed: make(chan time.Duration, 4), block: true}
	s := &Scheduler{admission: g, store: db, jobs: &liveCPUFixture{pool: argoworkflow.CPUAdmission{Milli: 9600}}}
	// Shorten the production two-minute retry ceiling to six seconds so this
	// regression demonstrates escape from the five-second cycle promptly.
	ctx := context.WithValue(t.Context(), databaseRetryKey{}, &databaseRetryScope{lifetime: t.Context(), owner: s, maximum: 6 * time.Second})
	start := time.Now()
	s.refreshCPUReservations(ctx)
	if elapsed := time.Since(start); elapsed > 5500*time.Millisecond {
		t.Fatalf("five-second observation cycle took %s because storage detached its deadline", elapsed)
	}
}

type r3ReviewReadStore struct{ SchedulerStore }

func (s *r3ReviewReadStore) Run(_ context.Context, id string) (model.Run, error) {
	return model.Run{ID: id}, nil
}

func TestReviewR3RefreshConcurrentPreparation(t *testing.T) {
	for attempt := 0; attempt < 10000; attempt++ {
		g := newAdmissionGate()
		s := &Scheduler{admission: g, store: &r3ReviewReadStore{}, jobs: &liveCPUFixture{}}
		var reservations []*admissionReservation
		for i := int64(1); i <= 1; i++ {
			reservations = append(reservations, g.reserveRun(model.Run{ID: fmt.Sprintf("run-%d", i), QueueSequence: i}))
		}
		// The background pass can capture this candidate between reserveRun and
		// reserveAdmission setting poolKnown. The dispatch goroutine then finishes
		// pool initialization before launching the preparation worker. Captured
		// candidates are not rechecked for eligibility in refreshLiveReservations.
		candidates := append([]*admissionReservation(nil), reservations...)
		g.mu.Lock()
		reservations[0].poolKnown = true
		g.mu.Unlock()
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() { <-start; s.refreshLiveReservations(t.Context(), candidates) })
		wg.Go(func() {
			<-start
			for _, r := range reservations {
				_, err := r.acquireRequest(t.Context(), admissionRequest{RunID: fmt.Sprintf("run-%d", r.sequence)})
				if err != nil {
					t.Error(err)
				}
			}
		})
		close(start)
		wg.Wait()
	}
}

type r3ReviewDelayedLive struct {
	liveCPUFixture
	entered, proceed chan struct{}
}

func (j *r3ReviewDelayedLive) LivePodRequests(context.Context, model.Run) (*argoworkflow.PodRequests, error) {
	close(j.entered)
	<-j.proceed
	return &argoworkflow.PodRequests{CPUMilli: 0}, nil
}
func TestReviewR3StaleObservationDoesNotAlterReplacement(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	run := model.Run{ID: "same-run", QueueSequence: 1}
	old, _ := g.adopt(run)
	old.poolKnown = true
	j := &r3ReviewDelayedLive{liveCPUFixture: liveCPUFixture{pool: argoworkflow.CPUAdmission{Milli: 9600}}, entered: make(chan struct{}), proceed: make(chan struct{})}
	s := &Scheduler{admission: g, store: &r3ReviewReadStore{}, jobs: j}
	done := make(chan struct{})
	go func() { s.refreshCPUReservations(t.Context()); close(done) }()
	<-j.entered
	old.release()
	fresh, _ := g.adopt(run)
	fresh.poolKnown = true
	fresh.cpu = g.defaultPool()
	close(j.proceed)
	<-done
	if g.reservations[run.QueueSequence] != fresh || g.credit(fresh, g.pool(fresh)) != 9600 || fresh.released {
		t.Fatal("stale read modified replacement reservation")
	}
}

type r3ReviewMixedLatency struct {
	liveCPUFixture
	mu                           sync.Mutex
	healthyCalls, healthySuccess int
}

func (j *r3ReviewMixedLatency) LivePodRequests(ctx context.Context, run model.Run) (*argoworkflow.PodRequests, error) {
	switch run.ID {
	case "run-1", "run-2", "run-3", "run-4":
		<-ctx.Done()
		return nil, ctx.Err()
	case "run-5":
		j.mu.Lock()
		j.healthyCalls++
		j.mu.Unlock()
		timer := time.NewTimer(1500 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			j.mu.Lock()
			j.healthySuccess++
			j.mu.Unlock()
			return &argoworkflow.PodRequests{CPUMilli: 1000, Pods: 1}, nil
		}
	default:
		return &argoworkflow.PodRequests{}, nil
	}
}
func TestReviewR3RotationDoesNotStarvePartialReadBehindFastSurvivor(t *testing.T) {
	g := newAdmissionGate()
	g.cpuMilli = 9600
	j := &r3ReviewMixedLatency{liveCPUFixture: liveCPUFixture{pool: argoworkflow.CPUAdmission{Milli: 9600}}}
	s := &Scheduler{admission: g, store: &r3ReviewReadStore{}, jobs: j}
	var healthy *admissionReservation
	for i := int64(1); i <= 6; i++ {
		r, _ := g.adopt(model.Run{ID: fmt.Sprintf("run-%d", i), QueueSequence: i})
		r.cpu = g.defaultPool()
		r.poolKnown = true
		if i <= 4 {
			r.unknownAdopted = false
			r.trackLive = true
			r.requests = &argoworkflow.PodRequests{CPUMilli: 1000}
		}
		if i == 5 {
			healthy = r
		}
	}
	for cycle := 1; cycle <= 3; cycle++ {
		s.refreshCPUReservations(t.Context())
		t.Logf("cycle=%d cursor=%d healthy_credit=%d", cycle, g.lastCPURefresh, g.credit(healthy, g.pool(healthy)))
	}
	waiter := reviewPrepared(g, 7, 500, model.PriorityStandard)
	if j.healthySuccess == 0 || !waiter.granted {
		t.Fatalf("healthy 1.5-second read starved: calls=%d success=%d cursor=%d credit=%d waiterGranted=%v", j.healthyCalls, j.healthySuccess, g.lastCPURefresh, g.credit(healthy, g.pool(healthy)), waiter.granted)
	}
}

// A cycle cutoff postpones observation; it is not evidence that the previous
// successful observation failed. This applies to initial sizing as well.
func TestCPURefreshCycleCutoffKeepsPreviousObservation(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			g := newAdmissionGate()
			g.cpuMilli = 9600
			r, err := g.adopt(model.Run{ID: "owned", QueueSequence: 1})
			if err != nil {
				t.Fatal(err)
			}
			previous := &argoworkflow.PodRequests{CPUMilli: 1500, Pods: 1}
			r.poolKnown = true
			r.cpu = g.defaultPool()
			r.liveRequests = previous
			r.sizingPending = pending
			j := &r4CanceledLive{liveCPUFixture: liveCPUFixture{pool: g.defaultPool()}}
			s := &Scheduler{admission: g, store: &r3ReviewReadStore{}, jobs: j}
			cycle, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer stop()
			production := context.WithValue(cycle, databaseRetryKey{}, &databaseRetryScope{lifetime: t.Context(), owner: s})
			s.refreshLiveReservations(production, []*admissionReservation{r})
			if r.liveRequests != previous || r.liveReadFailed || r.sizingPending != pending || g.lastCPURefresh != 0 {
				t.Fatalf("cycle cutoff changed cached observation: live=%+v failed=%v pending=%v cursor=%d", r.liveRequests, r.liveReadFailed, r.sizingPending, g.lastCPURefresh)
			}
		})
	}
}

type r4CanceledLive struct{ liveCPUFixture }

func (j *r4CanceledLive) LivePodRequests(ctx context.Context, _ model.Run) (*argoworkflow.PodRequests, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
