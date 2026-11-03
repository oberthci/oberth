package service

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestSchedulerPressureBlocksSecondRunWithinBudget(t *testing.T) {
	fixture := newConcurrencyFixture(t, periapsis.M, 2)
	gate := fixture.scheduler.admission
	config := DefaultAdmissionPressureConfig()
	// Sources run under the admission mutex. The first grant makes pressure
	// high, even if the second candidate is in the same scheduling pass.
	high := true
	config.Source = func() (model.PressureSample, error) {
		var sample model.PressureSample
		if gate.runs > 0 && high {
			sample.CPU.SomeAvg10 = 60
		}
		return sample, nil
	}
	config.Logf = func(string, ...any) {}
	gate.pressure = newPressureCheck(config)
	fixture.enqueue(t, 0)
	fixture.enqueue(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- fixture.scheduler.Run(ctx) }()
	defer func() {
		close(fixture.jobs.release)
		cancel()
		select {
		case <-finished:
		case <-time.After(testStartTimeout(15 * time.Second)):
			t.Error("scheduler did not stop")
		}
	}()
	deadline := time.NewTimer(testStartTimeout(15 * time.Second))
	defer deadline.Stop()
	select {
	case <-fixture.jobs.admitted:
	case err := <-finished:
		t.Fatalf("scheduler stopped: %v", err)
	case <-deadline.C:
		t.Fatal("first run never admitted")
	}
	poll := time.NewTicker(testTimeout(10 * time.Millisecond))
	defer poll.Stop()
	for {
		state := fixture.scheduler.AdmissionState()
		blocked := false
		for _, r := range state.Reservations {
			if r.Pressure != nil {
				blocked = true
			}
		}
		if blocked {
			if state.Runs != 1 || state.Used+2 > state.Capacity || !state.Pressure.Holding {
				t.Fatalf("budget should fit second run: %+v", state)
			}
			break
		}
		select {
		case <-fixture.jobs.admitted:
			t.Fatal("pressure allowed second run within budget")
		case err := <-finished:
			t.Fatalf("scheduler stopped: %v", err)
		case <-deadline.C:
			t.Fatalf("second run never reached pressure gate: %+v", state)
		case <-poll.C:
		}
	}
	// A new low reading recovers without waiting for the running job to finish.
	gate.mu.Lock()
	high = false
	gate.grantLocked()
	gate.mu.Unlock()
	select {
	case <-fixture.jobs.admitted:
	case err := <-finished:
		t.Fatalf("scheduler stopped: %v", err)
	case <-deadline.C:
		t.Fatal("second run did not recover with spare budget")
	}
}
