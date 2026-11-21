package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShutdownSharesTwentyFiveSecondBudget(t *testing.T) {
	f := fixtureServer(t)
	var mu sync.Mutex
	var durations []time.Duration
	var root time.Time
	f.deps.timeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		mu.Lock()
		defer mu.Unlock()
		if len(durations) > 0 {
			if deadline, ok := parent.Deadline(); !ok || !deadline.Equal(root) {
				t.Error("shutdown phase escaped total deadline")
			}
		}
		ctx, cancel := context.WithTimeout(parent, d)
		if len(durations) == 0 {
			root, _ = ctx.Deadline()
		}
		durations = append(durations, d)
		return ctx, cancel
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := f.start(ctx)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(durations) < 3 || durations[0] != 25*time.Second || durations[1] != 15*time.Second {
		t.Fatalf("wrong shutdown budgets: %v", durations)
	}
	for _, d := range durations[2:] {
		if d > 10*time.Second {
			t.Fatal("part exceeded its share")
		}
	}
}
func TestCloseTimeoutKeepsDependenciesOpen(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseClose := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseClose()
	store := make(chan struct{})
	life := newLifecycle(t.Context(), []Part{{Name: "store", Close: func(context.Context) error { close(store); return nil }}, {Name: "stuck close", Close: func(context.Context) error { close(entered); <-release; return nil }}})
	expired, cancel := context.WithCancel(t.Context())
	life.timeout = func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return context.WithCancel(expired)
	}
	trace := &serverTrace{ready: make(chan struct{})}
	life.diagnostic = trace
	done := make(chan error, 1)
	go func() { done <- life.stop(t.Context()) }()
	<-entered
	cancel()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stuck close not reported")
		}
	case <-timer.C:
		releaseClose()
		<-done
		t.Fatal("stuck close blocked shutdown")
	}
	select {
	case <-store:
		t.Fatal("store closed beneath unfinished Close")
	default:
	}
	if !strings.Contains(trace.String(), "stuck close") {
		t.Fatal("timed-out close not named")
	}
}
func TestStartupFailureAfterDrainIsPreserved(t *testing.T) {
	f := fixtureServer(t)
	entered := make(chan struct{})
	failure := errors.New("recovery refused during shutdown")
	original := f.deps.constructors.ServiceNew
	f.deps.constructors.ServiceNew = func(ctx context.Context, c *composition) (Part, error) {
		p, err := original(ctx, c)
		p.Recover = func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			<-f.listener(f.config.Listen.Source).closed
			return failure
		}
		return p, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, f.config, f.deps, f.trace) }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, failure) {
		t.Fatalf("late startup failure lost: %v", err)
	}
	if f.c.store.Readiness(t.Context()) == nil {
		t.Fatal("failed start left store open")
	}
}
