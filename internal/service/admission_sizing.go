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
	return argoworkflow.CPUAdmission{Milli: s.admission.cpuMilli}, nil
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
	pool := argoworkflow.CPUAdmission{Milli: s.admission.cpuMilli}
	if sizing, ok := s.jobs.(interface {
		AdoptedAdmissionCPU(context.Context, model.Run) (argoworkflow.CPUAdmission, error)
	}); ok {
		var err error
		pool, err = sizing.AdoptedAdmissionCPU(ctx, run)
		if err != nil {
			log.Printf("run %s: adopted CPU pool unavailable: %v", run.ID, err)
		}
	}
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	r.cpu, r.poolKnown = pool, true
	r.requests = clampAdmissionRequests(queuePodRequests(demand), pool.Milli)
	r.unknownAdopted = demand == nil || demand.CPUMilli <= 0
}

// refreshAdmission reads held attempts outside the gate lock. Failed reads retain
// the previous reservation; continued attempts regain their full CPU credit.
func (s *Scheduler) refreshAdmission(ctx context.Context) {
	gate := s.admission
	gate.mu.Lock()
	refresh := gate.lastHoldRefresh.IsZero() || gate.clock().Sub(gate.lastHoldRefresh) >= 15*time.Second
	if refresh {
		gate.lastHoldRefresh = gate.clock()
	}
	gate.mu.Unlock()
	if holds, ok := s.jobs.(interface {
		ListAssistHolds(context.Context, string) ([]model.AssistHold, error)
	}); ok && refresh {
		gate.mu.Lock()
		var candidates []*admissionReservation
		for _, r := range gate.reservations {
			if r.granted && !r.released && r.assistRequested {
				candidates = append(candidates, r)
			}
		}
		gate.mu.Unlock()
		for _, r := range candidates {
			readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			run, err := s.database().Run(readCtx, r.runID)
			var since time.Time
			if err == nil {
				var found []model.AssistHold
				found, err = holds.ListAssistHolds(readCtx, run.WorkflowName)
				for _, hold := range found {
					if !assistReleased(run.Assist, hold.Pod) && (since.IsZero() || hold.HeldSince.Before(since)) {
						since = hold.HeldSince
					}
				}
			}
			cancel()
			if err == nil {
				gate.mu.Lock()
				r.heldSince = since
				gate.mu.Unlock()
			}
		}
	}
	gate.mu.Lock()
	gate.grantLocked()
	gate.mu.Unlock()
}
