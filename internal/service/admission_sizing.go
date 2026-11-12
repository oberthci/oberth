package service

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func (s *Scheduler) admissionCPU(ctx context.Context, request WorkflowRequest) (argoworkflow.CPUAdmission, error) {
	if sizing, ok := s.jobs.(interface {
		AdmissionCPU(context.Context, WorkflowRequest) (argoworkflow.CPUAdmission, error)
	}); ok {
		return sizing.AdmissionCPU(ctx, request)
	}
	return s.admission.defaultPool(), nil
}

func (s *Scheduler) reservationCPU(ctx context.Context, run model.Run) (argoworkflow.CPUAdmission, bool) {
	pool := s.admission.defaultPool()
	if sizing, ok := s.jobs.(interface {
		AdoptedAdmissionCPU(context.Context, model.Run) (argoworkflow.CPUAdmission, error)
	}); ok {
		current, err := sizing.AdoptedAdmissionCPU(ctx, run)
		if err != nil {
			log.Printf("run %s: CPU pool unavailable: %v", run.ID, err)
			return pool, false
		}
		pool = current
	}
	return pool.Canonical(), true
}
func (s *Scheduler) readLiveRequests(ctx context.Context, run model.Run) *argoworkflow.PodRequests {
	if reader, ok := s.jobs.(interface {
		LivePodRequests(context.Context, model.Run) (*argoworkflow.PodRequests, error)
	}); ok {
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var requests *argoworkflow.PodRequests
		err := runDispatchExternal(readCtx, func(ctx context.Context) error {
			var err error
			requests, err = reader.LivePodRequests(ctx, run)
			return err
		})
		if err == nil {
			return requests
		}
		log.Printf("run %s: live CPU requests unavailable: %v", run.ID, err)
	}
	return nil
}
func (s *Scheduler) sizeAdoptedReservation(ctx context.Context, run model.Run, r *admissionReservation) {
	var demand *argoworkflow.PodDemand
	if sizing, ok := s.jobs.(interface {
		AdoptedPodDemand(context.Context, model.Run) (*argoworkflow.PodDemand, error)
	}); ok {
		err := runDispatchExternal(ctx, func(ctx context.Context) error {
			var err error
			demand, err = sizing.AdoptedPodDemand(ctx, run)
			return err
		})
		if err != nil {
			log.Printf("run %s: adopted CPU demand unknown: %v", run.ID, err)
			demand = nil
		}
	}
	pool, known := s.reservationCPU(ctx, run)
	unknown := demand == nil || demand.CPUMilli <= 0
	var live *argoworkflow.PodRequests
	if unknown {
		live = s.readLiveRequests(ctx, run)
	}
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	r.sizingPending = false
	r.cpu, r.poolKnown = pool, known
	r.cpuUncertain = !known
	r.requests = queuePodRequests(demand)
	r.unknownAdopted = unknown
	r.trackLive = unknown
	r.liveRequests = live
}

func (s *Scheduler) refreshAdmission(ctx context.Context) {
	gate := s.admission
	if sizing, ok := s.jobs.(interface {
		AdmissionCPUPools(context.Context) []argoworkflow.CPUAdmission
	}); ok {
		pools := sizing.AdmissionCPUPools(ctx)
		gate.mu.Lock()
		if gate.pools == nil {
			gate.pools = map[string]argoworkflow.CPUAdmission{}
		}
		seen := map[string]argoworkflow.CPUAdmission{}
		for _, pool := range pools {
			pool = pool.Canonical()
			if previous, exists := seen[pool.Pool]; exists && previous != pool {
				log.Printf("CPU capacity pool %q has conflicting observations; retaining first snapshot", pool.Pool)
				continue
			}
			seen[pool.Pool] = pool
			gate.pools[pool.Pool] = pool
		}
		gate.mu.Unlock()
	}
	gate.mu.Lock()
	gate.grantLocked()
	gate.mu.Unlock()
}

// Startup restores conservative credits without adding a live API read per
// owned Workflow. The background observer replaces them after recovery.
func (s *Scheduler) initializeAdoptedCPU(ctx context.Context, run model.Run, r *admissionReservation) {
	pool, known := s.reservationCPU(ctx, run)
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	r.cpu, r.poolKnown, r.cpuUncertain = pool, known, !known
	r.sizingPending, r.unknownAdopted, r.trackLive = true, true, true
}

func (s *Scheduler) runAdmissionRefresh(ctx context.Context) {
	ctx = withoutDispatchProgress(ctx)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		s.refreshCPUReservations(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) refreshCPUReservations(ctx context.Context) {
	gate := s.admission
	gate.mu.Lock()
	var candidates []*admissionReservation
	for _, r := range gate.reservations {
		if !r.released && (r.granted && (r.assistRequested || r.unknownAdopted || r.trackLive || r.sizingPending) || !r.poolKnown) {
			candidates = append(candidates, r)
		}
	}
	gate.mu.Unlock()
	s.refreshLiveReservations(ctx, candidates)
	s.signalQueue()
}
func (s *Scheduler) refreshReservationCPU(ctx context.Context, r *admissionReservation) {
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	s.admission.mu.Lock()
	runID := r.runID
	s.admission.mu.Unlock()
	run, err := s.database().Run(readCtx, runID)
	if err != nil {
		if ctx.Err() == nil {
			s.clearLiveCPU(r)
		}
		return
	}
	pool, known := s.reservationCPU(readCtx, run)
	var since time.Time
	if run.AssistRequested {
		if reader, ok := s.jobs.(interface {
			ListAssistHolds(context.Context, string) ([]model.AssistHold, error)
		}); ok {
			var holds []model.AssistHold
			err := runDispatchExternal(readCtx, func(ctx context.Context) error {
				var err error
				holds, err = reader.ListAssistHolds(ctx, run.WorkflowName)
				return err
			})
			if err != nil {
				if ctx.Err() == nil {
					s.clearLiveCPU(r)
				}
				return
			}
			for _, hold := range holds {
				if !assistReleased(run.Assist, hold.Pod) && (since.IsZero() || hold.HeldSince.Before(since)) {
					since = hold.HeldSince
				}
			}
		}
	}
	gate := s.admission
	gate.mu.Lock()
	unknown := r.unknownAdopted || r.trackLive
	gate.mu.Unlock()
	var live *argoworkflow.PodRequests
	if unknown || !since.IsZero() {
		live = s.readLiveRequests(readCtx, run)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	r.cpu, r.poolKnown = pool, known
	r.cpuUncertain = !known
	r.heldSince = since
	r.trackLive = unknown || !since.IsZero()
	r.liveReadFailed = (unknown || !since.IsZero()) && live == nil
	if r.liveReadFailed {
		gate.fallbackLiveCPULocked(r)
	} else {
		r.liveRequests = live
	}
}

func (s *Scheduler) clearLiveCPU(r *admissionReservation) {
	s.admission.mu.Lock()
	r.liveReadFailed = true
	s.admission.fallbackLiveCPULocked(r)
	s.admission.mu.Unlock()
}

// Failed reads cannot reduce previously observed excess resident demand.
func (gate *admissionGate) fallbackLiveCPULocked(r *admissionReservation) {
	previous := r.liveRequests
	r.liveRequests = nil
	if previous != nil && previous.CPUMilli > gate.credit(r, gate.pool(r)) {
		r.liveRequests = previous
	}
}

func (s *Scheduler) reserveAdmission(ctx context.Context, run model.Run) *admissionReservation {
	r := s.admission.reserveRun(run)
	pool, known := s.reservationCPU(ctx, run)
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	r.cpu, r.poolKnown, r.cpuUncertain = pool, known, !known
	s.admission.grantLocked()
	return r
}

// Keep the dispatcher live even when many owned Workflows cannot be read.
// Rotate by reservation sequence so each survivor eventually gets a refresh.
func (s *Scheduler) refreshLiveReservations(ctx context.Context, candidates []*admissionReservation) {
	// Observations are read-only. They must retain their caller's deadlines,
	// rather than borrowing an owner's detached mutation/finalization retries.
	ctx = context.WithValue(ctx, databaseRetryKey{}, (*databaseRetryScope)(nil))
	gate := s.admission
	gate.mu.Lock()
	after := gate.lastCPURefresh
	gate.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].sequence, candidates[j].sequence
		if (a > after) != (b > after) {
			return a > after
		}
		return a < b
	})
	refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Two workers share a single five-second cycle budget. Each API operation
	// retains its two-second deadline; no goroutine is created per owned run.
	tasks := make(chan int)
	completed := make(chan int, len(candidates))
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			for index := range tasks {
				r := candidates[index]
				if refreshCtx.Err() != nil {
					continue
				}
				gate.mu.Lock()
				pending, runID := r.sizingPending, r.runID
				gate.mu.Unlock()
				if pending {
					readCtx, stop := context.WithTimeout(refreshCtx, 2*time.Second)
					run, err := s.database().Run(readCtx, runID)
					if err == nil {
						s.sizeAdoptedReservation(readCtx, run, r)
					}
					stop()
				} else {
					s.refreshReservationCPU(refreshCtx, r)
				}
				if refreshCtx.Err() == nil {
					completed <- index
				}
			}
		})
	}
	for index := range candidates {
		if refreshCtx.Err() != nil {
			break
		}
		select {
		case tasks <- index:
		case <-refreshCtx.Done():
		}
	}
	close(tasks)
	workers.Wait()
	close(completed)
	finished := make([]bool, len(candidates))
	for index := range completed {
		finished[index] = true
	}
	last := -1
	for index, done := range finished {
		if !done {
			break
		}
		last = index
	}
	// Advance only over the completed prefix. A later fast observation cannot
	// move the cursor past an earlier read cut short by the cycle deadline.
	if last >= 0 {
		gate.mu.Lock()
		gate.lastCPURefresh = candidates[last].sequence
		gate.mu.Unlock()
	}
}
