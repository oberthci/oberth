package service

import (
	"sort"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// Aging raises a normal run one class, into high, at its own queue position:
// an explicit highest bump always precedes it, and inside high the order is
// FIFO, so an aged run precedes high work accepted after it and follows high
// work accepted before it.
func TestPriorityReviewAgedNormalJoinsHighFIFOBelowExplicitHighest(t *testing.T) {
	now := time.Now()
	highBefore := &admissionReservation{priority: model.PriorityPromotion, order: 1, sequence: 1, queuedAt: now.Add(-10 * time.Minute)}
	aged := &admissionReservation{priority: model.PriorityNormal, order: 2, sequence: 2, queuedAt: now.Add(-time.Hour)}
	highAfter := &admissionReservation{priority: model.PriorityPromotion, order: 3, sequence: 3, queuedAt: now}
	fresh := &admissionReservation{priority: model.PriorityNormal, order: 4, sequence: 4, queuedAt: now}
	explicit := &admissionReservation{priority: model.PriorityHighest, priorityReason: "admin bump: release blocker", order: 5, sequence: 5, queuedAt: now}
	if p := aged.effectivePriority(now); p != model.PriorityPromotion {
		t.Fatalf("aged normal effective priority = %s, want high", p)
	}
	if p := highBefore.effectivePriority(now.Add(2 * time.Hour)); p != model.PriorityPromotion {
		t.Fatalf("aged high effective priority = %s, want high (never highest)", p)
	}
	waiters := []*admissionReservation{fresh, explicit, highAfter, aged, highBefore}
	sort.SliceStable(waiters, func(i, j int) bool { return admissionBeforeAt(waiters[i], waiters[j], now) })
	want := []*admissionReservation{explicit, highBefore, aged, highAfter, fresh}
	for i := range want {
		if waiters[i] != want[i] {
			t.Fatalf("admission order at %d = sequence %d, want %d", i, waiters[i].sequence, want[i].sequence)
		}
	}
	for _, pair := range [][2]*admissionReservation{{explicit, aged}, {highBefore, aged}, {aged, highAfter}, {aged, fresh}} {
		if !admissionBeforeAt(pair[0], pair[1], now) || admissionBeforeAt(pair[1], pair[0], now) {
			t.Errorf("sequence %d should precede sequence %d", pair[0].sequence, pair[1].sequence)
		}
	}
}
