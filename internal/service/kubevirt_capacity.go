package service

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// KubeVirtResources binds one run's declared demand to its repository grant,
// capped by the server ceiling. It is an admission resource, not a run type.
type KubeVirtResources struct {
	Demand  argoworkflow.VMIDemand
	Limit   argoworkflow.VMIDemand
	Receipt *argoworkflow.VMIDemandReceipt
}

func (resources KubeVirtResources) Validate() error {
	demand, limit := resources.Demand, resources.Limit
	if resources.Receipt != nil && resources.Receipt.Summary().Demand != demand {
		return fmt.Errorf("service: KubeVirt reservation disagrees with demand receipt")
	}
	if demand.VMs < 0 || demand.Cores < 0 || demand.MemoryBytes < 0 ||
		limit.VMs < 0 || limit.Cores < 0 || limit.MemoryBytes < 0 ||
		(demand.VMs == 0 && (demand.Cores != 0 || demand.MemoryBytes != 0)) ||
		(demand.VMs > 0 && (demand.Cores == 0 || demand.MemoryBytes == 0)) {
		return fmt.Errorf("service: invalid KubeVirt resource demand")
	}
	if !demand.Fits(limit) {
		return fmt.Errorf("service: VMI run budget exceeded: %d VMs, %d cores, %d memory bytes; grant allows %d VMs, %d cores, %d memory bytes",
			demand.VMs, demand.Cores, demand.MemoryBytes, limit.VMs, limit.Cores, limit.MemoryBytes)
	}
	return nil
}

// PipelineKubeVirtSizer reads the same immutable document used for submission.
// Engines without VMI resources need not implement it.
type PipelineKubeVirtSizer interface {
	PipelineKubeVirt(context.Context, JobRequest) (KubeVirtResources, error)
}

func (scheduler *Scheduler) pipelineKubeVirt(ctx context.Context, request JobRequest) (KubeVirtResources, error) {
	engine := any(scheduler.jobs)
	if request.Run.Release {
		if _, ok := scheduler.releaseJobs.(PipelineKubeVirtSizer); ok {
			engine = scheduler.releaseJobs
		}
	}
	sizer, ok := engine.(PipelineKubeVirtSizer)
	if !ok {
		return KubeVirtResources{}, nil
	}
	resources, err := sizer.PipelineKubeVirt(ctx, request)
	if err != nil {
		return KubeVirtResources{}, err
	}
	return resources, resources.Validate()
}

func (gate *weightedAdmission) kubeVirtUsedLocked(repoID int64) argoworkflow.VMIDemand {
	var used argoworkflow.VMIDemand
	for _, reservation := range gate.reservations {
		if reservation.repoID != repoID || !reservation.granted || reservation.released {
			continue
		}
		demand := reservation.kubeVirt.Demand
		used.VMs += demand.VMs
		used.Cores += demand.Cores
		used.MemoryBytes += demand.MemoryBytes
	}
	return used
}

func (gate *weightedAdmission) kubeVirtFitsLocked(reservation *admissionReservation) bool {
	if reservation.kubeVirt.Demand.VMs == 0 {
		return true
	}
	used := gate.kubeVirtUsedLocked(reservation.repoID)
	demand := reservation.kubeVirt.Demand
	used.VMs += demand.VMs
	used.Cores += demand.Cores
	used.MemoryBytes += demand.MemoryBytes
	return used.Fits(reservation.kubeVirt.Limit)
}

// A capacity waiter keeps its FIFO place within the repository while unrelated
// work passes it. A later smaller VMI run cannot consume the capacity it needs.
func (gate *weightedAdmission) kubeVirtBlockedLocked(reservation *admissionReservation) bool {
	if !reservation.prepared || reservation.granted || reservation.kubeVirt.Demand.VMs == 0 {
		return false
	}
	if !gate.kubeVirtFitsLocked(reservation) {
		return true
	}
	for _, earlier := range gate.waiters {
		if earlier == reservation {
			break
		}
		if earlier.repoID != reservation.repoID || !earlier.prepared || earlier.vmParked {
			continue
		}
		if holder, ahead := gate.groupBlockerLocked(earlier); holder != "" || ahead != "" {
			continue
		}
		if !gate.kubeVirtFitsLocked(earlier) {
			return true
		}
	}
	return false
}

func (reservation *admissionReservation) markSubmitted() {
	reservation.gate.mu.Lock()
	defer reservation.gate.mu.Unlock()
	reservation.submitted = true
}

// Accepted deletion releases a submitted Workflow synchronously, without waiting
// for its observer. During creation, Delete may only record cancellation intent;
// the creating worker must finish cleanup before its reservation can be released.
func (gate *weightedAdmission) releaseDeletedRun(runID string) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	for _, reservation := range gate.reservations {
		if reservation.runID == runID && reservation.granted && reservation.submitted {
			reservation.retained = false
			gate.releaseLocked(reservation)
			return
		}
	}
}

// AdoptedKubeVirtSizer reads demand pinned by JobAdopter to the existing
// Workflow. Recovery must not size already-running work from a fresh checkout.
type AdoptedKubeVirtSizer interface {
	AdoptedKubeVirtDemand(model.Run) (argoworkflow.VMIDemand, error)
}

func (scheduler *Scheduler) adoptedKubeVirtDemand(run model.Run) (argoworkflow.VMIDemand, error) {
	if sizer, ok := scheduler.jobs.(AdoptedKubeVirtSizer); ok {
		return sizer.AdoptedKubeVirtDemand(run)
	}
	if _, ok := scheduler.jobs.(PipelineKubeVirtSizer); ok {
		return argoworkflow.VMIDemand{}, fmt.Errorf("service: VMI-capable engine cannot restore adopted demand")
	}
	return argoworkflow.VMIDemand{}, nil
}
