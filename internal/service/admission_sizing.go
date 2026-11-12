package service

import (
	"context"
	"log"
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
		requests, err := reader.LivePodRequests(readCtx, run)
		if err == nil {
			return requests
		}
		log.Printf("run %s: live CPU requests unavailable; reserving whole pool: %v", run.ID, err)
	}
	return nil
}
func (s *Scheduler) sizeAdoptedReservation(ctx context.Context, run model.Run, r *admissionReservation) {
	var demand *argoworkflow.PodDemand
	if sizing, ok := s.jobs.(interface {
		AdoptedPodDemand(model.Run) (*argoworkflow.PodDemand, error)
	}); ok {
		var err error
		demand, err = sizing.AdoptedPodDemand(run)
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
		for _, pool := range pools {
			pool = pool.Canonical()
			gate.pools[pool.Pool] = pool
		}
		gate.mu.Unlock()
	}
	gate.mu.Lock()
	refresh := gate.lastHoldRefresh.IsZero() || gate.clock().Sub(gate.lastHoldRefresh) >= 15*time.Second
	var candidates []*admissionReservation
	if refresh {
		gate.lastHoldRefresh = gate.clock()
		for _, r := range gate.reservations {
			if !r.released && (r.granted && (r.assistRequested || r.unknownAdopted || r.trackLive) || !r.poolKnown) {
				candidates = append(candidates, r)
			}
		}
	}
	gate.mu.Unlock()
	for _, r := range candidates {
		s.refreshReservationCPU(ctx, r)
	}
	gate.mu.Lock()
	gate.grantLocked()
	gate.mu.Unlock()
}
func (s *Scheduler) refreshReservationCPU(ctx context.Context, r *admissionReservation) {
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	run, err := s.database().Run(readCtx, r.runID)
	if err != nil {
		s.clearLiveCPU(r)
		return
	}
	pool, known := s.reservationCPU(readCtx, run)
	var since time.Time
	if run.AssistRequested {
		if reader, ok := s.jobs.(interface {
			ListAssistHolds(context.Context, string) ([]model.AssistHold, error)
		}); ok {
			holds, err := reader.ListAssistHolds(readCtx, run.WorkflowName)
			if err != nil {
				s.clearLiveCPU(r)
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
	r.cpu, r.poolKnown = pool, known
	r.cpuUncertain = !known
	r.heldSince = since
	r.trackLive = unknown || !since.IsZero()
	r.liveRequests = live
}

func (s *Scheduler) clearLiveCPU(r *admissionReservation) {
	s.admission.mu.Lock()
	r.liveRequests = nil
	s.admission.mu.Unlock()
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
