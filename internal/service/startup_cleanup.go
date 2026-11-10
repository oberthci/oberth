package service

import (
	"context"
	"errors"
	"log"
	"time"
)

const startupCleanupAttemptBudget = 2 * time.Minute

// Only external deletion failures are retryable at the startup boundary.
// Store invariant and schema failures retain their fatal behavior.
type pendingJobDeletionError struct{ error }

func (e *pendingJobDeletionError) Unwrap() error { return e.error }

// cancellationRetryMaximum caps the steady-state cancellation pass backoff.
const cancellationRetryMaximum = 30 * time.Second

// onlyPendingJobDeletions reports whether err consists solely of Workflow
// deletions the API refused, which leave their obligations pending. Any
// other cause, alone or joined, is not retryable here.
func onlyPendingJobDeletions(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !onlyPendingJobDeletions(cause) {
				return false
			}
		}
		return true
	}
	var pending *pendingJobDeletionError
	return errors.As(err, &pending)
}

// Startup cleanup is idempotent and owns durable obligations. A temporarily
// unavailable deletion API must leave the server alive and unready, rather
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
		if !onlyPendingJobDeletions(err) {
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
