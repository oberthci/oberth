package service

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type capacityPriorityStore struct{ SchedulerStore }

func (*capacityPriorityStore) RunsByIDs(context.Context, []string) (map[string]model.Run, error) {
	return nil, nil
}
func (*capacityPriorityStore) NextQueuedRun(context.Context) (model.Run, error) {
	return model.Run{ID: "release", QueueSequence: 100, Priority: model.PriorityHighest, QueuedAt: time.Now()}, nil
}

func runtimePriorityFixture(t *testing.T) *Scheduler {
	t.Helper()
	s, _ := runtimeCapacityFixture(t, 2)
	s.store = &capacityPriorityStore{}
	// A preparing ordinary claim keeps priority dispatch active without granting
	// or releasing work while the capacity watcher changes its ceiling.
	waiting := s.admission.reserveRun(model.Run{ID: "ordinary", QueueSequence: 1, Priority: model.PriorityNormal, QueuedAt: time.Now()})
	t.Cleanup(waiting.cancel)
	return s
}
func TestRuntimeCapacityPriorityDispatchUsesLiveCeiling(t *testing.T) {
	s := runtimePriorityFixture(t)
	for _, tc := range []struct{ jobs, below, at int }{{4, 13, 14}, {1, 4, 5}} {
		writeCapacity(t, s, fmt.Sprint(tc.jobs))
		s.refreshRuntimeCapacity(t.Context())
		if available, err := s.priorityDispatchAvailable(t.Context(), tc.below); err != nil || !available {
			t.Fatal("priority dispatch kept boot ceiling below live bound", tc, available, err)
		}
		if available, err := s.priorityDispatchAvailable(t.Context(), tc.at); err != nil || available {
			t.Fatal("priority dispatch passed live bound", tc, available, err)
		}
	}
}
func TestRuntimeCapacityWatcherBesidePriorityDispatch(t *testing.T) {
	s := runtimePriorityFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time)
	watchDone, dispatchDone := make(chan struct{}), make(chan struct{})
	failures := make(chan error, 1)
	var calls atomic.Int64
	go func() { defer close(watchDone); s.watchRuntimeCapacity(ctx, ticks) }()
	go func() {
		defer close(dispatchDone)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			available, err := s.priorityDispatchAvailable(ctx, 0)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				failures <- fmt.Errorf("priority dispatch stopped: %w", err)
				return
			}
			if !available {
				failures <- errors.New("priority dispatch stopped below the live ceiling")
				return
			}
			calls.Add(1)
			runtime.Gosched()
		}
	}()
	defer func() {
		cancel()
		for _, done := range []chan struct{}{watchDone, dispatchDone} {
			select {
			case <-done:
			case <-time.After(testStartTimeout(time.Second)):
				t.Error("capacity watcher/dispatch did not stop")
			}
		}
	}()
	for i := range 64 {
		jobs := 1 + i%2
		writeCapacity(t, s, fmt.Sprint(jobs))
		select {
		case ticks <- time.Now():
		case <-time.After(testStartTimeout(time.Second)):
			t.Fatal("watcher did not sample")
		}
		deadline := time.Now().Add(testStartTimeout(time.Second))
		for s.AdmissionState().Configuration.MaxConcurrentJobs != jobs {
			if time.Now().After(deadline) {
				t.Fatal("watcher did not publish live capacity")
			}
			runtime.Gosched()
		}
	}
	if calls.Load() == 0 {
		t.Fatal("dispatch never overlapped watcher")
	}
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
}
