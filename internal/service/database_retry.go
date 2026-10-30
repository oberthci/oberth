package service

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const databaseAttemptBudget = 10 * time.Second
const databaseRetryMinimum = 250 * time.Millisecond
const databaseRetryMaximum = 5 * time.Second

type databaseRetryKey struct{}
type databaseRetryScope struct {
	lifetime context.Context
	owner    *Scheduler
}

// Retry only homogeneous transient storage failures. In particular, joining
// BUSY/deadline with an invariant violation must never hide the latter.
func transientDatabaseError(err error) bool {
	return transientDatabaseAttemptError(err, false)
}

// IsTransientDatabaseError classifies a completed storage operation, not an
// arbitrary external operation or error text. It is also used by parent-run
// observers, which must not turn an unavailable read into cancellation.
func IsTransientDatabaseError(err error) bool { return transientDatabaseError(err) }

func preflightDatabaseError(err error) bool {
	return (errors.Is(err, context.DeadlineExceeded) || store.IsTransientLockError(err)) && transientDatabaseAttemptError(err, true)
}

func transientDatabaseAttemptError(err error, expired bool) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !transientDatabaseAttemptError(child, expired) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return transientDatabaseAttemptError(wrapped.Unwrap(), expired)
	}
	// database/sql may report an already-rolled-back transaction, and SQLite
	// may report INTERRUPT, when our own attempt deadline fired. They are not
	// transient in the absence of that specific deadline evidence.
	if expired {
		var code interface{ Code() int }
		if errors.Is(err, sql.ErrTxDone) || errors.Is(err, context.Canceled) || (errors.As(err, &code) && code.Code()&255 == 9) {
			return true
		}
	}
	return errors.Is(err, context.DeadlineExceeded) || store.IsTransientLockError(err)
}

// A background owner keeps the exact storage continuation pending, rather than
// replaying its surrounding Workflow creation, observation or Git mutation.
// Each attempt has a fresh 10s budget; exponential backoff is capped at 5s.
// Retries last only for the scheduler lifetime. Short cleanup/attempt contexts
// must not turn a dependency outage into a fatal worker completion; shutdown
// still uses the caller's original bounded cleanup context without retries.
// No retry scope is installed on client request contexts.
func retryDatabase[T any](ctx context.Context, operation string, attempt func(context.Context) (T, error)) (T, error) {
	return retryDatabaseClassified(ctx, operation, attempt, transientDatabaseAttemptError)
}

func retryDatabaseClassified[T any](ctx context.Context, operation string, attempt func(context.Context) (T, error), transient func(error, bool) bool) (T, error) {
	scope, ok := ctx.Value(databaseRetryKey{}).(*databaseRetryScope)
	if !ok || scope.lifetime.Err() != nil {
		return attempt(ctx)
	}
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(scope.lifetime, cancel)
	defer func() { stop(); cancel() }()
	delay := databaseRetryMinimum
	var nextLog time.Time
	for {
		// A storage call that ignores its deadline remains a local wedge.
		// Suspend only the intentional retry sleep after a completed failure.
		beatDispatch(ctx)
		attemptCtx, cancelAttempt := context.WithTimeout(base, databaseAttemptBudget)
		value, err := attempt(attemptCtx)
		expired := errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancelAttempt()
		if err == nil {
			return value, err
		}
		if base.Err() != nil {
			return value, base.Err()
		}
		if !transient(err, expired) {
			return value, err
		}
		total := scope.owner.databaseRetryCount.Add(1)
		if now := time.Now(); !now.Before(nextLog) {
			log.Printf("WARN: scheduler database operation=%s retry_pending=true retries_total=%d retry_in=%s error=%q", operation, total, delay, err)
			nextLog = now.Add(30 * time.Second)
		}
		if err := waitDatabaseRetry(base, delay); err != nil {
			return value, err
		}
		delay = min(2*delay, databaseRetryMaximum)
	}
}

func waitDatabaseRetry(ctx context.Context, delay time.Duration) error {
	resume := suspendDispatch(ctx)
	defer resume()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryDatabaseError(ctx context.Context, operation string, attempt func(context.Context) error) error {
	_, err := retryDatabase(ctx, operation, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, attempt(ctx)
	})
	return err
}

// DatabaseRetries counts failed background storage attempts, including retries
// in recovery and finalization. It never counts ordinary client-request errors.
func (scheduler *Scheduler) DatabaseRetries() uint64 { return scheduler.databaseRetryCount.Load() }

// RetryRecovery is for idempotent owner activation/replay before Run starts.
// Permanent verification failures still reach the fatal component channel.
func (scheduler *Scheduler) RetryRecovery(ctx context.Context, operation string, recover func(context.Context) error) error {
	ctx = context.WithValue(ctx, databaseRetryKey{}, &databaseRetryScope{lifetime: ctx, owner: scheduler})
	return retryDatabaseError(ctx, operation, recover)
}

type databaseAuditor struct{ Auditor }

func (auditor databaseAuditor) AppendAuditAction(ctx context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	return retryDatabase(ctx, "AppendAuditAction", func(ctx context.Context) (model.AuditAction, error) {
		return auditor.Auditor.AppendAuditAction(ctx, spec)
	})
}
