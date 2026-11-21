package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestStartupJoinedCancellationKeepsIndependentFailure(t *testing.T) {
	failure := errors.New("recovery failed before cancellation")
	cases := map[string]error{"plain": failure, "joined": errors.Join(failure, context.Canceled), "wrapped join": fmt.Errorf("recovery: %w", errors.Join(context.Canceled, failure)), "nested": errors.Join(fmt.Errorf("stop: %w", context.Canceled), errors.Join(failure, context.Canceled))}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			life := newLifecycle(ctx, nil)
			life.abortStart()
			if err := life.phaseError(result); !errors.Is(err, failure) {
				t.Error("phase discarded independent failure", err)
			}
			s := &serving{starting: make(chan error, 1)}
			s.starting <- result
			if joined, err := s.joinStartup(t.Context()); !joined || !errors.Is(err, failure) {
				t.Error("shutdown discarded independent failure", err)
			}
			s.starting = make(chan error, 1)
			s.starting <- result
			// Keep the completion boundary deterministic rather than racing ctx.Done.
			if err := s.start(t.Context()); !errors.Is(err, failure) {
				t.Error("startup discarded independent failure", err)
			}
		})
	}
}

func TestStartupWrappedCancellationAloneStopsNormally(t *testing.T) {
	for _, result := range []error{context.Canceled, fmt.Errorf("owner stop: %w", context.Canceled), errors.Join(context.Canceled, fmt.Errorf("owner stop: %w", context.Canceled))} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		life := newLifecycle(ctx, nil)
		life.abortStart()
		if err := life.phaseError(result); !errors.Is(err, context.Canceled) {
			t.Fatal("phase cancellation lost", err)
		}
		s := &serving{starting: make(chan error, 1)}
		s.starting <- result
		if joined, err := s.joinStartup(t.Context()); !joined || err != nil {
			t.Fatal("cancellation-only completion failed", err)
		}
	}
}

func TestJoinedCallbackFailureSurvivesShutdown(t *testing.T) {
	failure := errors.New("submission recovery refused")
	for _, phase := range []string{"Ready", "Recover"} {
		t.Run(phase, func(t *testing.T) {
			f := newStartupShutdownFixture(t, phase, errors.Join(failure, context.Canceled))
			done := make(chan error, 1)
			go func() { done <- f.server.close(t.Context()) }()
			<-f.server.life.phase.Done()
			close(f.release)
			if err := <-done; !errors.Is(err, failure) {
				t.Error("finished callback failure hidden", err)
			}
			<-f.returned
		})
	}
}

func TestServePreservesCancelledRecoveryFailure(t *testing.T) {
	f := fixtureServer(t)
	failure := errors.New("submission recovery failed")
	entered := make(chan struct{})
	original := f.deps.constructors.WiringNew
	f.deps.constructors.WiringNew = func(ctx context.Context, c *composition) (Part, error) {
		part, err := original(ctx, c)
		part.Recover = func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			return errors.Join(failure, fmt.Errorf("recovery stopped: %w", ctx.Err()))
		}
		return part, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, f.config, f.deps, f.trace) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("serve ended before recovery", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, failure) {
		t.Fatal("serve lost recovery failure", err)
	}
}

// Cancel at the Err check so select must consume the already finished callback.
type cancelAtErrorCheck struct {
	context.Context
	cancel context.CancelFunc
}

func (c cancelAtErrorCheck) Err() error { c.cancel(); return c.Context.Err() }

func TestStartupCompletionKeepsFailureWhenSignalArrives(t *testing.T) {
	failure := errors.New("finished recovery failure")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &serving{starting: make(chan error, 1)}
	s.starting <- errors.Join(failure, context.Canceled)
	if err := s.start(cancelAtErrorCheck{ctx, cancel}); !errors.Is(err, failure) {
		t.Fatal("signal hid completed failure", err)
	}
}
