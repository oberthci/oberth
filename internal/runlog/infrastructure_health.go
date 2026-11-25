package runlog

import (
	"slices"
	"sync"
)

// Missed writes retain only the latest exact delivery result in memory. A lost
// success masks stale failures; a lost failure is uncertain, never infrastructure.
// When a run's lost results exceed a bound, only that run becomes uncertain;
// other runs keep their own evidence. This fallback is independent of the
// volume and disappears on process exit. Beyond 4096 uncertain runs the oldest
// is forgotten.
type deliveryEvidenceHealth struct {
	mu        sync.Mutex
	runs      map[string][]InfrastructureReceipt
	count     int
	uncertain []string // oldest first; cleared by purge
}

const (
	maxVolatileDeliveryReceipts = 1024
	maxVolatileRunReceipts      = 64
	maxUncertainDeliveryRuns    = 4096
)

func laterDelivery(old, next InfrastructureReceipt) bool {
	return old.FinishedAt.After(next.FinishedAt) || old.FinishedAt.Equal(next.FinishedAt) && old.Delivered && !next.Delivered
}

// remember records the outcome of a write. masks reports whether a durable
// failure for the same delivery may exist; a lost success without one changes
// nothing and is not retained.
func (h *deliveryEvidenceHealth) remember(r InfrastructureReceipt, writeErr error, masks bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if slices.Contains(h.uncertain, r.RunID) {
		return
	}
	if h.runs == nil {
		h.runs = map[string][]InfrastructureReceipt{}
	}
	events := h.runs[r.RunID]
	i := slices.IndexFunc(events, r.sameDelivery)
	if i >= 0 && laterDelivery(events[i], r) {
		return
	}
	if writeErr == nil {
		if i >= 0 {
			h.drop(r.RunID, i)
		}
		return
	}
	r.Uncertain = !r.Delivered
	if i >= 0 {
		events[i] = r
		return
	}
	if r.Delivered && !masks {
		return
	}
	if len(events) >= maxVolatileRunReceipts || h.count >= maxVolatileDeliveryReceipts {
		h.markUncertain(r.RunID)
		return
	}
	h.runs[r.RunID] = append(events, r)
	h.count++
}

func (h *deliveryEvidenceHealth) drop(id string, i int) {
	h.runs[id] = slices.Delete(h.runs[id], i, i+1)
	h.count--
	if len(h.runs[id]) == 0 {
		delete(h.runs, id)
	}
}

func (h *deliveryEvidenceHealth) markUncertain(id string) {
	h.count -= len(h.runs[id])
	delete(h.runs, id)
	if len(h.uncertain) >= maxUncertainDeliveryRuns {
		h.uncertain = slices.Delete(h.uncertain, 0, 1)
	}
	h.uncertain = append(h.uncertain, id)
}

func (h *deliveryEvidenceHealth) merge(id string, durable []InfrastructureReceipt) ([]InfrastructureReceipt, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if slices.Contains(h.uncertain, id) {
		return nil, storage(deliveryEvidenceUnavailableReason, nil)
	}
	merged := slices.Clone(durable)
	for _, r := range h.runs[id] {
		i := slices.IndexFunc(merged, r.sameDelivery)
		if i < 0 {
			merged = append(merged, r)
		} else if !laterDelivery(merged[i], r) {
			merged[i] = r
		}
	}
	return slices.DeleteFunc(merged, func(r InfrastructureReceipt) bool { return r.Delivered }), nil
}

func (h *deliveryEvidenceHealth) purge(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count -= len(h.runs[id])
	delete(h.runs, id)
	h.uncertain = slices.DeleteFunc(h.uncertain, func(u string) bool { return u == id })
}
