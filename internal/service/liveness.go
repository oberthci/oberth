package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/liveness"
)

// Live checks only in-memory local progress. Recovery and external waits are
// excluded; readiness still gates traffic while those dependencies are down.
func (scheduler *Scheduler) Live() error {
	if err := scheduler.dispatchProgress.Check(); err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	return nil
}

// Promotion planning waits only on external services; it has no local worker loop.
func (service *API) Live() error { return nil }

type dispatchProgressKey struct{}

// Only the dispatch goroutine receives this context; run background tasks must never
// renew or suspend the dispatch heartbeat on its behalf.
func withDispatchProgress(ctx context.Context, heartbeat *liveness.Heartbeat) context.Context {
	return context.WithValue(ctx, dispatchProgressKey{}, heartbeat)
}

// WithoutCancel retains values, so explicitly hide the dispatch-only capability
// at every handoff. A distinct sentinel also makes typed-nil mistakes harmless.
func withoutDispatchProgress(ctx context.Context) context.Context {
	return context.WithValue(ctx, dispatchProgressKey{}, struct{}{})
}

func suspendDispatch(ctx context.Context) func() {
	if heartbeat, ok := ctx.Value(dispatchProgressKey{}).(*liveness.Heartbeat); ok && heartbeat != nil {
		return heartbeat.Suspend()
	}
	return func() {}
}

// awaitDispatchMutation retries external witness failures without letting an
// intentional audit hold age into a local liveness failure.
func (scheduler *Scheduler) awaitDispatchMutation(ctx context.Context) error {
	for {
		resume := suspendDispatch(ctx)
		err := scheduler.requireMutation(ctx)
		resume()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !errors.Is(err, errMutationBlocked) {
			return err
		}
		timer := time.NewTimer(mutationGateRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func beatDispatch(ctx context.Context) {
	if heartbeat, ok := ctx.Value(dispatchProgressKey{}).(*liveness.Heartbeat); ok && heartbeat != nil {
		heartbeat.Beat()
	}
}

// runDispatchExternal excludes one external call. Keep local DB work and
// background task locks outside this scope so they can still time out as real wedges.
func runDispatchExternal(ctx context.Context, operation func(context.Context) error) error {
	resume := suspendDispatch(ctx)
	defer resume()
	return operation(ctx)
}

// WalkLiveness exposes only process-local progress snapshots to the background
// logger; it never takes background task locks or probes dependencies.
func (scheduler *Scheduler) WalkLiveness(visit func(string, *liveness.Heartbeat)) {
	visit("scheduler", &scheduler.dispatchProgress)
}

func (service *API) WalkLiveness(_ func(string, *liveness.Heartbeat)) {}
