package service

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestPriorityReviewNoBackfillBehindPriorityHead(t *testing.T) {
	g := newWeightedAdmission(6)
	g.maxHeadWait = 20 * time.Minute
	// A long L and a short S consume four units. Another S would stop the L
	// head starting when the short running S finishes.
	g.reservations[1] = &admissionReservation{sequence: 1, size: periapsis.L, weight: 3, granted: true}
	g.reservations[2] = &admissionReservation{sequence: 2, size: periapsis.S, weight: 1, granted: true}
	head := &admissionReservation{sequence: 3, size: periapsis.L, weight: 3, priority: model.PriorityHighest}
	candidate := &admissionReservation{sequence: 4, size: periapsis.S, weight: 1, priority: model.PriorityHighest}
	if g.priorityHeadFitsAlongsideLocked(head, candidate) {
		t.Fatal("backfill can delay priority head behind running work")
	}
}

func TestPriorityReviewBackfillPolicyForAllClasses(t *testing.T) {
	g := newWeightedAdmission(8)
	for _, priority := range []model.RunPriority{model.PriorityNormal, model.PriorityHigh, model.PriorityHighest} {
		head := &admissionReservation{priority: priority, weight: 1, size: periapsis.S}
		candidate := &admissionReservation{weight: 1, size: periapsis.S}
		if got, want := g.priorityHeadFitsAlongsideLocked(head, candidate), priority == model.PriorityNormal; got != want {
			t.Errorf("class %s backfill=%v, want %v", priority, got, want)
		}
	}
}
