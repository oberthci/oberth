package service

import "github.com/oberthci/oberth/internal/model"

// Ignore the work that is already running: once it frees capacity, the
// backfilled work must leave enough declared budget and run slots for head.
// Normal queues retain the existing bounded backfill behavior.
func (g *weightedAdmission) priorityHeadFitsAlongsideLocked(head, candidate *admissionReservation) bool {
	if head.effectivePriority(g.clock()) == model.PriorityNormal {
		return true
	}
	if (g.maxRuns > 0 && g.maxRuns < 2) || (g.resources.IORuns > 0 && g.resources.IORuns < 2) {
		return false
	}
	if (!g.usesDeclarations(head) && g.weights.wholeBudget(head.size)) || (!g.usesDeclarations(candidate) && g.weights.wholeBudget(candidate.size)) {
		return false
	}
	// numericFitsLocked only reads declarations and granted reservations.
	held := *candidate
	held.granted = true
	held.released = false
	budget := &weightedAdmission{capacity: g.capacity, resources: g.resources, reservations: map[int64]*admissionReservation{candidate.sequence: &held}}
	weight, runs := head.weight+candidate.weight, 2
	for seq, r := range g.reservations {
		if r != candidate && r.granted && !r.released && r.backfilled {
			budget.reservations[seq] = r
			weight += r.weight
			runs++
		}
	}
	if (g.maxRuns > 0 && runs > g.maxRuns) || (g.resources.IORuns > 0 && runs > g.resources.IORuns) {
		return false
	}
	return weight <= g.capacity && budget.numericFitsLocked(head)
}
