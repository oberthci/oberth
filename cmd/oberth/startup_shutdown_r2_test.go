package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type startupShutdownFixture struct {
	server            *serving
	fixture           *serverFixture
	release, returned chan struct{}
}

func newStartupShutdownFixture(t *testing.T, phase string, failure error) startupShutdownFixture {
	t.Helper()
	f := fixtureServer(t)
	life := newLifecycle(t.Context(), nil)
	c := &composition{config: f.config, clock: f.deps.clock, health: &health{}, diagnostic: f.trace}
	parts, err := openStorage(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range parts {
		life.add(part)
	}
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	callback := func(context.Context) error {
		close(entered)
		<-release
		f.trace.record("startup callback returned")
		return failure
	}
	service := Part{Name: "service", Shutdown: stopService, Close: func(context.Context) error { f.trace.record("close service"); return nil }}
	if phase == "Ready" {
		service.Ready = callback
	} else {
		service.Recover = callback
	}
	life.add(service)
	life.add(Part{Name: "independent listener", Stop: func() { f.trace.record("stop listener") }})
	f.deps.timeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if d == 25*time.Second {
			d = time.Second
		}
		return context.WithTimeout(ctx, d)
	}
	s := newServing(c, life, f.deps)
	s.starting = make(chan error, 1)
	completion := s.starting
	go func() { completion <- life.start(); close(returned) }()
	<-entered
	return startupShutdownFixture{s, f, release, returned}
}
func TestStartupCallbacksRetainDependenciesAfterDeadline(t *testing.T) {
	for _, phase := range []string{"Ready", "Recover"} {
		t.Run(phase, func(t *testing.T) {
			f := newStartupShutdownFixture(t, phase, nil)
			result := make(chan error, 1)
			go func() { result <- f.server.close(t.Context()) }()
			failure := <-result
			storage := f.server.c.store.Readiness(t.Context())
			events := strings.Join(f.fixture.trace.snapshot(), "\n")
			close(f.release)
			<-f.returned
			if err := f.server.life.stop(t.Context()); err != nil {
				t.Error("cleanup", err)
			}
			if failure == nil || !strings.Contains(failure.Error(), "Part: startup") {
				t.Error("startup deadline failure missing", failure)
			}
			if strings.Contains(events, "close service") || storage != nil {
				t.Error("startup callback lost its dependencies", events, storage)
			}
			if strings.Count(events, "stop listener") != 1 {
				t.Error("independent listener did not stop once")
			}
		})
	}
}
func TestStartupCallbackFailureJoinsBeforeDependenciesClose(t *testing.T) {
	failure := errors.New("startup callback refused")
	for _, phase := range []string{"Ready", "Recover"} {
		t.Run(phase, func(t *testing.T) {
			f := newStartupShutdownFixture(t, phase, failure)
			result := make(chan error, 1)
			go func() { result <- f.server.close(t.Context()) }()
			<-f.server.life.phase.Done()
			close(f.release)
			if err := <-result; !errors.Is(err, failure) {
				t.Error("callback failure lost", err)
			}
			<-f.returned
			events := strings.Join(f.fixture.trace.snapshot(), "\n")
			returned, closed := strings.Index(events, "startup callback returned"), strings.Index(events, "close service")
			if returned < 0 || closed < returned {
				t.Error("service closed before callback returned", events)
			}
			if strings.Count(events, "stop listener") != 1 {
				t.Error("listener stopped more than once", events)
			}
			if err := f.server.c.store.Readiness(t.Context()); err == nil {
				t.Error("joined callback kept store open")
			}
		})
	}
}
