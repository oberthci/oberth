package service

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestFourClassesUrgencyPassesPromotionsWithoutPreemptingRunningWork(t *testing.T) {
	g := newWeightedAdmission(4)
	holder := g.reserve(1)
	release, err := holder.acquire(context.Background(), pipeline.XL)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
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
			t.Fatal("waiting urgency preempted running work or bypassed resource cap")
		}
	}
	release()
	want := append(append(append(high, promotion...), standard...), low...)
	for i, r := range want {
		if !granted(g, r) {
			t.Fatalf("four-class FIFO failed at %d (priority %s)", i, r.priority)
		}
		g.mu.Lock()
		if g.runs != 1 || g.used != 4 {
			t.Errorf("admission constraints changed: runs=%d used=%d", g.runs, g.used)
		}
		g.mu.Unlock()
		r.release()
	}
}
