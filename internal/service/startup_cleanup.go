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

// deferRefusedDeletions makes a worker's cancellation pass that failed only on
// Workflow deletions the API refused non-fatal. Those obligations stay
// pending in the store; the dispatch loop's cancellation pass owns the retry
// and its backoff, so the worker logs, wakes that pass and ends normally
// instead of ending the server. Every other error is returned unchanged.
//
// A caller whose own Workflow may still be alive retains its admission
// reservation first. The pass that completes that run's obligation releases
// it through releaseDeletedRun; if the pass already completed it, the
// reservation is released and retain is a no-op, so the slot is released
// exactly once.
func (scheduler *Scheduler) deferRefusedDeletions(ctx context.Context, runID string, err error) error {
	if err == nil || !onlyPendingJobDeletions(err) || ctx.Err() != nil {
		return err
	}
	log.Printf("WARN: scheduler worker cancellation deletion_pending=true retry=dispatch_pass error=%q", err)
	scheduler.signalQueue()
	// The run is already preempted in the store; waiters see it now rather
	// than at the next unrelated run signal.
	scheduler.signals.NotifyRun(runID)
	return nil
}

// settlePodJournal prunes a terminal run's pod-log journal once its log
// projection is indexed: now when the Workflow is settled, or by the dispatch
// pass that deletes it when the worker deferred that deletion.
func (scheduler *Scheduler) settlePodJournal(ctx context.Context, runID, logPath string, projectionReady, workflowSettled, deletionDeferred bool) {
	switch {
	case projectionReady && workflowSettled:
		scheduler.pruneTerminalPodJournal(ctx, runID, logPath)
	case projectionReady && deletionDeferred:
		scheduler.deferPodJournalPrune(ctx, runID, logPath)
	}
}

// deferPodJournalPrune hands a deferred run's pod-log journal to the dispatch
// pass that deletes its Workflow. When that pass already completed the run's
// obligation, the journal is pruned here instead; either way exactly once.
func (scheduler *Scheduler) deferPodJournalPrune(ctx context.Context, runID, logPath string) {
	scheduler.deferredJournalMu.Lock()
	if scheduler.deferredJournals == nil {
		scheduler.deferredJournals = map[string]string{}
	}
	scheduler.deferredJournals[runID] = logPath
	scheduler.deferredJournalMu.Unlock()
	pending, err := scheduler.database().PendingRunCancellations(context.WithoutCancel(ctx))
	if err != nil {
		return // the pass, or evidence retention, prunes it later
	}
	for _, cancellation := range pending {
		if cancellation.RunID == runID {
			return
		}
	}
	scheduler.pruneDeferredPodJournal(ctx, runID)
}

func (scheduler *Scheduler) pruneDeferredPodJournal(ctx context.Context, runID string) {
	scheduler.deferredJournalMu.Lock()
	logPath, ok := scheduler.deferredJournals[runID]
	delete(scheduler.deferredJournals, runID)
	scheduler.deferredJournalMu.Unlock()
	if ok {
		scheduler.pruneTerminalPodJournal(ctx, runID, logPath)
	}
}

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
