package service

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestEASYEarlyReturnPreservesWaitingHeadForecast(t *testing.T) {
	for _, reason := range []string{"unprepared claim", "pressure deferral"} {
		t.Run(reason, func(t *testing.T) {
			g, clock, running := easyGate(t)
			head := pending(t, g, 2, easyRequest("head", 8200, 0))
			t.Cleanup(head.cancel)
			safe := pending(t, g, 3, easyRequest("safe", 1100, time.Minute))
			if !granted(g, safe) {
				t.Fatal("initial backfill did not establish a forecast")
			}
			safe.release()
			original := head.backfillDeadline
			if original.IsZero() {
				t.Fatal("head has no forecast")
			}

			var ahead *admissionReservation
			if reason == "unprepared claim" {
				ahead = g.reserveRun(model.Run{ID: "preparing", QueueSequence: 4, Priority: model.PriorityHighest, QueuedAt: clock.now})
			} else {
				// Prepare behind the head, then raise priority while pressure
				// holds this otherwise eligible waiter ahead of it.
				ahead = pending(t, g, 4, easyRequest("pressure-held", 4000, 0))
				g.mu.Lock()
				ahead.priority = model.PriorityHighest
				g.pressure = newPressureCheck(AdmissionPressureConfig{IOFullAvg60Max: 30, Hysteresis: 10, MaxDefer: 20 * time.Minute,
					Source: func() (model.PressureSample, error) {
						sample := model.PressureSample{}
						sample.IO.FullAvg60 = 80
						return sample, nil
					}})
				g.grantLocked()
				g.mu.Unlock()
				if granted(g, ahead) || !ahead.pressureDeferred {
					t.Fatal("fixture did not stop on the earlier pressure-held waiter")
				}
			}
			t.Cleanup(ahead.cancel)
			if g.backfillHead != head || !head.backfillDeadline.Equal(original) {
				t.Error("early return discarded a still-waiting head's forecast")
			}

			clock.now = original.Add(time.Minute)
			g.mu.Lock()
			running.estimate = 2 * time.Hour
			g.pressure = nil
			g.mu.Unlock()
			ahead.cancel()
			later := pending(t, g, 5, easyRequest("later", 1100, time.Minute))
			t.Cleanup(later.cancel)
			if granted(g, later) || !head.backfillDeadline.Equal(original) {
				t.Fatal("early return allowed a later deadline and new backfill after overrun")
			}
		})
	}
}

func TestEASYRemovedHeadClearsForecastBeforeEarlyReturn(t *testing.T) {
	g, clock, _ := easyGate(t)
	head := pending(t, g, 2, easyRequest("head", 8200, 0))
	safe := pending(t, g, 3, easyRequest("safe", 1100, time.Minute))
	if !granted(g, safe) {
		t.Fatal("initial backfill did not establish a forecast")
	}
	safe.release()
	ahead := g.reserveRun(model.Run{ID: "preparing", QueueSequence: 4, Priority: model.PriorityHighest, QueuedAt: clock.now})
	t.Cleanup(ahead.cancel)
	head.cancel()
	if g.backfillHead != nil || !head.backfillDeadline.IsZero() {
		t.Fatal("removed head retained its forecast while another claim was preparing")
	}
}
