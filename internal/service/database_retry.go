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

// A returned preflight deadline authorizes exact, fail-closed cleanup, not DB
// replay. It may originate in Kubernetes or the executor as well as storage.
func recoverablePreflightError(err error) bool {
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
// Single storage calls have fresh 10s budgets; backoff is capped at 5s.
// Owner shutdown stops retries between attempts, never an in-flight detached
// cleanup/finalization. Calls starting after shutdown use the original context.
// No retry scope is installed on client request contexts.
func retryDatabase[T any](ctx context.Context, operation string, attempt func(context.Context) (T, error)) (T, error) {
	return retryDatabaseClassified(ctx, operation, databaseAttemptBudget, attempt, transientDatabaseAttemptError)
}

func retryDatabaseClassified[T any](ctx context.Context, operation string, budget time.Duration, attempt func(context.Context) (T, error), transient func(error, bool) bool) (T, error) {
	scope, ok := ctx.Value(databaseRetryKey{}).(*databaseRetryScope)
	if !ok || scope == nil || scope.lifetime.Err() != nil {
		return attempt(ctx)
	}
	// Composite operations retain their caller's cancellation/deadline. Only
	// storage attempts may detach a short caller context; lifetime is checked
	// between attempts so shutdown cannot abort deliberate drain work.
	base := ctx
	if budget > 0 {
		base = context.WithoutCancel(ctx)
	}
	delay := databaseRetryMinimum
	var nextLog time.Time
	for {
		// A storage call that ignores its deadline remains a local wedge.
		// Suspend only the intentional retry sleep after a completed failure.
		beatDispatch(ctx)
		attemptCtx := base
		cancelAttempt := func() {}
		if budget > 0 {
			attemptCtx, cancelAttempt = context.WithTimeout(base, budget)
		}
		value, err := attempt(attemptCtx)
		expired := errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancelAttempt()
		if err == nil {
			return value, err
		}
		if scope.lifetime.Err() != nil {
			return value, scope.lifetime.Err()
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
		if err := waitDatabaseRetry(ctx, scope.lifetime, delay); err != nil {
			return value, err
		}
		delay = min(2*delay, databaseRetryMaximum)
	}
}

func waitDatabaseRetry(ctx, lifetime context.Context, delay time.Duration) error {
	// Preserve dispatch progress values, but stop the backoff on owner shutdown.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(lifetime, cancel)
	defer func() { stop(); cancel() }()
	resume := suspendDispatch(ctx)
	defer resume()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return lifetime.Err()
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

// RetryRecovery scopes idempotent activation/replay without imposing a storage
// statement timeout on Git, Kubernetes or multi-statement recovery. Bare external
// deadlines are not database evidence; storage calls inside replay retry locally.
func (scheduler *Scheduler) RetryRecovery(ctx context.Context, operation string, recover func(context.Context) error) error {
	return scheduler.retryRecovery(ctx, operation, recover, func(err error, _ bool) bool {
		return transientLockError(err)
	})
}

// RetryStorageRecovery is for a known, idempotent, database-only transaction.
// It keeps the caller's operation budget, including long startup recovery.
func (scheduler *Scheduler) RetryStorageRecovery(ctx context.Context, operation string, recover func(context.Context) error) error {
	return scheduler.retryRecovery(ctx, operation, recover, transientDatabaseAttemptError)
}

func (scheduler *Scheduler) retryRecovery(ctx context.Context, operation string, recover func(context.Context) error, transient func(error, bool) bool) error {
	ctx = context.WithValue(ctx, databaseRetryKey{}, &databaseRetryScope{lifetime: ctx, owner: scheduler})
	_, err := retryDatabaseClassified(ctx, operation, 0, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, recover(ctx)
	}, transient)
	return err
}

// A composite operation may contain external deadlines. Only homogeneous,
// positively typed SQLite contention permits replay at that boundary.
func transientLockError(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !transientLockError(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return transientLockError(wrapped.Unwrap())
	}
	return store.IsTransientLockError(err)
}

type databaseAuditor struct{ Auditor }

func (auditor databaseAuditor) AppendAuditAction(ctx context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	return retryDatabase(ctx, "AppendAuditAction", func(ctx context.Context) (model.AuditAction, error) {
		return auditor.Auditor.AppendAuditAction(ctx, spec)
	})
}
