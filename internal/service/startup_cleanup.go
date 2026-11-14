package service

import (
	"context"
	"errors"
	"log"
	"time"
)

const startupCleanupAttemptBudget = 2 * time.Minute

// Only external Stop failures are retryable at the startup boundary.
// Store invariant and schema failures retain their fatal behavior.
type pendingWorkflowStopError struct{ error }

func (e *pendingWorkflowStopError) Unwrap() error { return e.error }

// deferRefusedStops makes a worker's cancellation pass that failed only on
// Workflow Stops the API refused non-fatal. Those obligations stay
// pending in the store; the dispatch loop's cancellation pass owns the retry
// and its backoff, so the worker logs, wakes that pass and ends normally
// instead of ending the server. Every other error is returned unchanged.
//
// The durable ledger retains native Stop work while a worker exits.
func (scheduler *Scheduler) deferRefusedStops(ctx context.Context, runID string, err error) error {
	if err == nil || !onlyPendingWorkflowStops(err) || ctx.Err() != nil {
		return err
	}
	log.Printf("WARN: scheduler worker cancellation stop_pending=true retry=dispatch_pass error=%q", err)
	scheduler.signalQueue()
	// The run is carrying Stop intent in the store; waiters see it now rather
	// than at the next unrelated run signal.
	scheduler.signals.NotifyRun(runID)
	return nil
}

// cancellationRetryMaximum caps the steady-state cancellation pass backoff.
const cancellationRetryMaximum = 30 * time.Second

// onlyPendingWorkflowStops reports whether err consists solely of Workflow
// Stops the API refused, which leave their obligations pending. Any
// other cause, alone or joined, is not retryable here.
func onlyPendingWorkflowStops(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !onlyPendingWorkflowStops(cause) {
				return false
			}
		}
		return true
	}
	var pending *pendingWorkflowStopError
	return errors.As(err, &pending)
}

// Startup cleanup is idempotent and owns durable obligations. A temporarily
// unavailable Stop API must leave the server alive and unready, rather
// than force another Recreate restart. Each attempt and its retry backoff are
// bounded; shutdown cancels both. No new run is dispatched until this returns.
func (scheduler *Scheduler) waitForStartupCleanup(ctx context.Context, operation string, cleanup func(context.Context) error) error {
	delay := databaseRetryMinimum
	var nextLog time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, startupCleanupAttemptBudget)
		err := cleanup(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !onlyPendingWorkflowStops(err) {
			return err
		}
		total := scheduler.databaseRetryCount.Add(1)
		if now := time.Now(); !now.Before(nextLog) {
			log.Printf("WARN: scheduler startup cleanup operation=%s recovery_pending=true ready=false retries_total=%d attempt_budget=%s retry_in=%s error=%q", operation, total, startupCleanupAttemptBudget, delay, err)
			nextLog = now.Add(30 * time.Second)
		}
		if err := waitDatabaseRetry(ctx, ctx, delay); err != nil {
			return err
		}
		delay = min(2*delay, databaseRetryMaximum)
	}
}
