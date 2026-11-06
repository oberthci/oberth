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

type queuedAdmissionMessage struct {
	Text     string
	Pressure bool
}

// Unclaimed rows can wait on the workflow ceiling without a prepared head.
func (scheduler *Scheduler) QueuedAdmissionMessage() queuedAdmissionMessage {
	if scheduler == nil || scheduler.admission == nil || !scheduler.admission.workflowOnly {
		return queuedAdmissionMessage{}
	}
	g := scheduler.admission
	g.mu.Lock()
	defer g.mu.Unlock()
	if p := g.pressureStateLocked(); p != nil && p.Holding {
		return queuedAdmissionMessage{Text: queuePressureReason(&model.PressureDeferral{Resource: p.Resource}), Pressure: true}
	}
	if g.maxRuns > 0 && g.runs >= g.maxRuns {
		return queuedAdmissionMessage{Text: fmt.Sprintf("%d of %d runs running", g.runs, g.maxRuns)}
	}
	return queuedAdmissionMessage{}
}

func (g *weightedAdmission) reportedLegacyResourcesLocked(r *admissionReservation) *model.LegacyResourceCharge {
	if g.workflowOnly {
		return nil
	}
	return r.legacyResources()
}
