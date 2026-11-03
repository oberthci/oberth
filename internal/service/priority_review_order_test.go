package service

import (
	"github.com/oberthci/oberth/internal/model"
	"testing"
	"time"
)

func TestPriorityReviewExplicitHighestBeforeAgedWork(t *testing.T) {
	now := time.Now()
	explicit := &admissionReservation{priority: model.PriorityHighest, order: 3, sequence: 3, queuedAt: now}
	high := &admissionReservation{priority: model.PriorityHigh, order: 2, sequence: 2, queuedAt: now.Add(-time.Hour)}
	normal := &admissionReservation{priority: model.PriorityNormal, order: 1, sequence: 1, queuedAt: now.Add(-time.Hour)}
	for _, pair := range [][2]*admissionReservation{{explicit, high}, {explicit, normal}, {high, normal}} {
		if !admissionBeforeAt(pair[0], pair[1], now) || admissionBeforeAt(pair[1], pair[0], now) {
			t.Errorf("base priority %s should precede %s at equal effective priority", pair[0].priority, pair[1].priority)
		}
	}
}
