package auditanchor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/testtimeout"
)

type deadlineContinuity struct {
	Continuity
	reconcile func(context.Context) error
}

func (c *deadlineContinuity) Reconcile(ctx context.Context, history []model.AuditWitness) error {
	if c.reconcile != nil {
		if err := c.reconcile(ctx); err != nil {
			return err
		}
	}
	return c.Continuity.Reconcile(ctx, history)
}

func deadlineTestManager(t *testing.T, timeout time.Duration) (*Manager, *deadlineContinuity) {
	t.Helper()
	now := time.Now()
	database, err := store.OpenAdminClient(t.Context(), filepath.Join(t.TempDir(), "audit.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	continuity := &deadlineContinuity{Continuity: newTestContinuity(t)}
	manager, err := NewManager(ManagerConfig{Store: database, Authority: &fakeAuthority{now: &now}, Witness: &fakeWitness{now: &now}, Continuity: continuity, Interval: time.Minute, MaxAge: time.Hour, Now: func() time.Time { return now }, GateTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	return manager, continuity
}

func TestMutationGateDeadlineBoundsExternalWork(t *testing.T) {
	manager, continuity := deadlineTestManager(t, 100*time.Millisecond)
	entered := false
	continuity.reconcile = func(ctx context.Context) error {
		entered = true
		<-ctx.Done()
		return ctx.Err()
	}
	started := time.Now()
	err := manager.AllowMutation(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || !entered || time.Since(started) > 3*time.Second {
		t.Fatalf("gate deadline: entered=%t duration=%s err=%v", entered, time.Since(started), err)
	}
	continuity.reconcile = nil
	// Give the lock-reuse probe the normal gate budget so reader startup on
	// a loaded host cannot obscure whether the expired attempt released it.
	manager.gateTimeout = defaultGateTimeout
	if err := manager.AllowMutation(t.Context()); err != nil {
		t.Fatalf("deadline failed to release refresh lock: %v", err)
	}
}

func TestMutationGateDeadlineBoundsRefreshQueue(t *testing.T) {
	manager, continuity := deadlineTestManager(t, 5*time.Second)
	entered := make(chan struct{})
	continuity.reconcile = func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() { firstDone <- manager.AllowMutation(firstCtx) }()
	select {
	case <-entered:
	case <-time.After(testtimeout.Start(3 * time.Second)):
		t.Fatal("first gate never entered reconciliation")
	}
	secondCtx, cancelSecond := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelSecond()
	err := manager.AllowMutation(secondCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued gate ignored deadline: %v", err)
	}
	select {
	case err := <-firstDone:
		t.Fatalf("queue test first gate finished early: %v", err)
	default:
	}
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first gate ignored cancellation: %v", err)
	}
	continuity.reconcile = nil
	if err := manager.AllowMutation(t.Context()); err != nil {
		t.Fatalf("refresh lock leaked: %v", err)
	}
}

func TestMutationGateTimeoutDefaultAndValidation(t *testing.T) {
	manager, _ := deadlineTestManager(t, 0)
	if manager.gateTimeout != 30*time.Second {
		t.Fatalf("default gate timeout = %s", manager.gateTimeout)
	}
	if _, err := NewManager(ManagerConfig{Store: manager.store, Interval: time.Minute, MaxAge: time.Hour, GateTimeout: -time.Second}); err == nil {
		t.Fatal("negative timeout accepted")
	}
}
