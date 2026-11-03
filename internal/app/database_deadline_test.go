package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type exactRecoveryStore struct {
	trustedRunStore
	requested []string
	cleaned   []string
	err       error
}

func (s *exactRecoveryStore) KVMPreflight(_ context.Context, id string) (vmrunner.KVMExecution, error) {
	s.requested = append(s.requested, id)
	return vmrunner.KVMExecution{Intent: vmrunner.KVMIntent{RunID: id}, Rejected: true, Refusal: &vmrunner.KVMRefusal{}}, s.err
}
func (s *exactRecoveryStore) CompleteKVMPreflightCleanup(_ context.Context, id, uid string) error {
	s.cleaned = append(s.cleaned, id)
	return nil
}
func TestDatabaseDeadlineTrustedRecoveryTouchesOnlyExactRun(t *testing.T) {
	s := &exactRecoveryStore{}
	p := &TrustedPreflights{store: s}
	if err := p.RecoverRun(t.Context(), "returned-owner"); err != nil {
		t.Fatal(err)
	}
	if len(s.requested) != 1 || s.requested[0] != "returned-owner" || len(s.cleaned) != 1 || s.cleaned[0] != "returned-owner" {
		t.Fatal("recovery enumerated unrelated live work", s.requested, s.cleaned)
	}
	s.err = context.DeadlineExceeded
	if err := p.RecoverRun(t.Context(), "unreadable"); !errors.Is(err, context.DeadlineExceeded) || len(s.cleaned) != 1 {
		t.Fatal("unreadable journal allowed cleanup", err, s.cleaned)
	}
}
func TestDatabaseDeadlineRetryCountInUnscopedStatus(t *testing.T) {
	h := Health{DatabaseRetries: func() uint64 { return 17 }}
	value, err := h.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if value.(HealthStatus).DatabaseRetries != 17 {
		t.Fatal("background retries missing from status", value)
	}
}

func (s *exactRecoveryStore) PendingKVMPreflights(context.Context) ([]vmrunner.KVMExecution, error) {
	return nil, errors.New("per-run recovery enumerated unrelated executions")
}

type parentDeadlineStore struct {
	trustedRunStore
	calls    int
	observed chan int
	hard     error
}

func (s *parentDeadlineStore) Run(ctx context.Context, id string) (model.Run, error) {
	s.calls++
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 6*time.Second {
		panic("unbounded parent observation")
	}
	s.observed <- s.calls
	if s.hard != nil {
		return model.Run{}, s.hard
	}
	switch s.calls {
	case 1:
		return model.Run{}, context.DeadlineExceeded
	case 2:
		return model.Run{}, parentBusyError{}
	case 3:
		return model.Run{ID: id, Status: model.RunRunning}, nil
	default:
		return model.Run{ID: id, Status: model.RunInterrupted}, nil
	}
}

type parentBusyError struct{}

func (parentBusyError) Code() int     { return 5 }
func (parentBusyError) Error() string { return "SQLite busy" }
func TestDatabaseDeadlineTrustedParentObserverRetriesBeforeCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time)
	s := &parentDeadlineStore{observed: make(chan int, 1)}
	p := &TrustedPreflights{store: s}
	done := make(chan struct{})
	var canceled int
	go func() { defer close(done); p.watchParentRun(ctx, "owned", ticks, func() { canceled++; cancel() }) }()
	for n := 1; n <= 4; n++ {
		select {
		case ticks <- time.Now():
		case <-done:
			t.Fatalf("DB read %d canceled a live parent", n)
		case <-time.After(10 * time.Second):
			t.Fatal("parent watcher stopped progressing")
		}
		select {
		case got := <-s.observed:
			if got != n {
				t.Fatal("unexpected read", got, n)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("missing parent observation")
		}
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal parent did not cancel preflight")
	}
	if canceled != 1 || s.calls != 4 {
		t.Fatal("transient reads became cancellation", canceled, s.calls)
	}
}
func TestDatabaseDeadlineTrustedParentHardFailureStillCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	s := &parentDeadlineStore{hard: errors.New("corrupt parent record"), observed: make(chan int, 1)}
	p := &TrustedPreflights{store: s}
	canceled := false
	p.watchParentRun(ctx, "owned", ticks, func() { canceled = true; cancel() })
	if !canceled || s.calls != 1 {
		t.Fatal("hard parent failure ignored", canceled, s.calls)
	}
}
