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
	// PSI lags pod startup: repeated samples stay moderate after the grant.
	// The gate must wait for settling rather than expect an immediate spike.
	cpu := 20.0
	offset := time.Duration(0)
	gate.now = func() time.Time { return time.Now().Add(offset) }
	config.Source = func() (model.PressureSample, error) {
		var sample model.PressureSample
		sample.CPU.SomeAvg10 = cpu
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
	// The next poll still sees lagged moderate PSI and must keep waiting.
	gate.mu.Lock()
	offset = 15 * time.Second
	gate.grantLocked()
	if gate.runs != 1 {
		t.Error("next poll admitted before the settle window")
	}
	// At 30 seconds the delayed pressure spike is finally visible.
	offset = 30 * time.Second
	cpu = 60
	gate.grantLocked()
	if gate.runs != 1 {
		t.Error("settled high pressure admitted the second run")
	}
	gate.mu.Unlock()
	// A later low reading recovers without finishing the running job.
	gate.mu.Lock()
	cpu = 19
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
