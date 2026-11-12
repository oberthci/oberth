package service

import "time"

const cpuBackfillWindow = 2 * time.Minute

type cpuDrain struct {
	head  *admissionReservation
	since time.Time
}

func (gate *admissionGate) refreshDrainsLocked(_ time.Time) {
	for pool, drain := range gate.drains {
		r := drain.head
		if r.granted || r.released || r.canceled || gate.adminHeldLocked(r) || gate.pool(r).Pool != pool {
			delete(gate.drains, pool)
		}
	}
}
func (gate *admissionGate) blockedCPUHeadLocked(r *admissionReservation, now time.Time) {
	pool := gate.pool(r).Pool
	if gate.drains == nil {
		gate.drains = map[string]cpuDrain{}
	}
	drain, found := gate.drains[pool]
	if !found {
		gate.drains[pool] = cpuDrain{r, now}
		return
	}
	// One clock belongs to the oldest blocked head in this pool; another
	// blocked waiter cannot restart or interleave its backfill window.
	if admissionBeforeAt(r, drain.head, now) {
		drain.head = r
		gate.drains[pool] = drain
	}
}
func (gate *admissionGate) drainingBehindLocked(r *admissionReservation, now time.Time) bool {
	drain, found := gate.drains[gate.pool(r).Pool]
	return found && r != drain.head && now.Sub(drain.since) >= cpuBackfillWindow && !admissionBeforeAt(r, drain.head, now) && gate.headCanDrainLocked(drain.head)
}

// A drain can recover finite work's credits, but cannot make an assisted hold,
// uncertain cleanup or an unreadable adopted workload finish. Keep backfilling
// until those credits change enough for this head to become attainable.
func (gate *admissionGate) headCanDrainLocked(head *admissionReservation) bool {
	pool := gate.pool(head)
	available := pool.Milli
	for _, r := range gate.reservations {
		if !r.granted || r.released || (!r.cpuUncertain && !gate.samePool(r, head)) {
			continue
		}
		if r.retained || !r.heldSince.IsZero() || ((r.unknownAdopted || r.trackLive) && (r.liveRequests == nil || r.liveReadFailed)) {
			available -= gate.credit(r, pool)
		}
	}
	return available >= gate.credit(head, pool)
}
