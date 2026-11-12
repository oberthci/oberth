package service

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestFourClassesUrgencyPassesPromotionsWithoutPreemptingRunningWork(t *testing.T) {
	g := newAdmissionGate()
	holder := g.reserve(1)
	release, err := holder.acquire(context.Background(), pipeline.XL)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	blockForLifecycleTest(g)
	var low, standard, promotion, high []*admissionReservation
	for i, name := range []model.RunPriority{"low", "standard", "promotion", "high", "low", "standard", "promotion", "high"} {
		r := priorityWaiter(g, int64(i+2), name, time.Now(), pipeline.XL)
		switch name {
		case "low":
			low = append(low, r)
		case "standard":
			standard = append(standard, r)
		case "promotion":
			promotion = append(promotion, r)
		case "high":
			high = append(high, r)
		}
		if granted(g, r) || !granted(g, holder) {
			t.Fatal("waiting urgency preempted running work or bypassed the disk floor")
		}
	}
	want := append(append(append(high, promotion...), standard...), low...)
	g.mu.Lock()
	g.sortWaitersLocked()
	for i, r := range want {
		if g.waiters[i] != r {
			t.Errorf("four-class FIFO failed at %d (priority %s)", i, r.priority)
		}
	}
	g.mu.Unlock()
	unblockLifecycleTest(g)
	for _, r := range want {
		if !granted(g, r) {
			t.Fatalf("waiter %s not admitted after the floor cleared", r.priority)
		}
		r.release()
	}
}
