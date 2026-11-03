package service

import (
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

func (g *weightedAdmission) reportedResourcesLocked(r *admissionReservation) *model.DeclaredResources {
	if g.workflowOnly {
		return nil
	}
	return r.declaredResources()
}

// Unclaimed rows can wait on the workflow ceiling without a prepared head.
func (scheduler *Scheduler) QueuedAdmissionMessage() string {
	if scheduler == nil || scheduler.admission == nil || !scheduler.admission.workflowOnly {
		return ""
	}
	g := scheduler.admission
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.maxRuns > 0 && g.runs >= g.maxRuns {
		return fmt.Sprintf("%d runs running", g.runs)
	}
	if p := g.pressureStateLocked(); p != nil && p.Holding {
		return (model.AdmissionBlocker{Pressure: &model.PressureDeferral{Resource: p.Resource}}).Reason()
	}
	return ""
}

func (g *weightedAdmission) reportedLegacyResourcesLocked(r *admissionReservation) *model.LegacyResourceCharge {
	if g.workflowOnly {
		return nil
	}
	return r.legacyResources()
}
