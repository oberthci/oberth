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
func (scheduler *Scheduler) Live() error { return scheduler.liveAt(time.Now()) }

func (scheduler *Scheduler) liveAt(now time.Time) error {
	if err := scheduler.dispatchProgress.CheckAt(now); err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	if err := scheduler.headProgress.CheckAt(now); err != nil {
		return fmt.Errorf("admission persistence: %w", err)
	}
	return nil
}

// Live checks every queued promotion independently, so one busy queue cannot
// mask a wedged worker in another. Planning/delivery use external services and
// are not included in the local queue-wait heartbeat.
func (service *API) Live() error { return service.liveAt(time.Now()) }

func (service *API) liveAt(now time.Time) error {
	var err error
	service.promotionProgress.Range(func(_, value any) bool {
		if checkErr := value.(*liveness.Heartbeat).CheckAt(now); checkErr != nil {
			err = fmt.Errorf("promotion queue: %w", checkErr)
			return false
		}
		return true
	})
	return err
}

type dispatchProgressKey struct{}

// Only the dispatch goroutine receives this context; run workers must never
// renew or suspend the dispatch heartbeat on its behalf.
func withDispatchProgress(ctx context.Context, heartbeat *liveness.Heartbeat) context.Context {
	return context.WithValue(ctx, dispatchProgressKey{}, heartbeat)
}

func suspendDispatch(ctx context.Context) func() {
	if heartbeat, ok := ctx.Value(dispatchProgressKey{}).(*liveness.Heartbeat); ok {
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
	if heartbeat, ok := ctx.Value(dispatchProgressKey{}).(*liveness.Heartbeat); ok {
		heartbeat.Beat()
	}
}

func (scheduler *Scheduler) awaitDispatchAdmission(ctx context.Context) error {
	resume := suspendDispatch(ctx)
	defer resume()
	return scheduler.awaitAdmission(ctx)
}

// runDispatchExternal excludes one external call. Keep local DB work and
// worker locks outside this scope so they can still time out as real wedges.
func runDispatchExternal(ctx context.Context, operation func(context.Context) error) error {
	resume := suspendDispatch(ctx)
	defer resume()
	return operation(ctx)
}
