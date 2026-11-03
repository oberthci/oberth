package service

import "github.com/oberthci/oberth/internal/model"

// v0.17.81 conservatively forbids every backfill behind a priority head.
// This is the integration seam for fix/ob-backfill: replace this helper with
// its EASY reservation rule, accounting for work still running at the head's
// estimated start. A total-capacity fit alone cannot prove a safe backfill.
// Normal heads retain the existing bounded backfill behavior.
func (g *weightedAdmission) priorityHeadFitsAlongsideLocked(head, _ *admissionReservation) bool {
	return head.effectivePriority(g.clock()) == model.PriorityNormal
}
