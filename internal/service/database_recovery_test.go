package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

func TestDatabaseDeadlineLongRecoveryKeepsOperationBudget(t *testing.T) {
	for _, storage := range []bool{false, true} {
		t.Run(map[bool]string{true: "owner-transaction", false: "git-replay"}[storage], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				scheduler := &Scheduler{}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				calls := 0
				operation := func(ctx context.Context) error {
					calls++
					select {
					case <-time.After(11 * time.Second):
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				recover := scheduler.RetryRecovery
				if storage {
					recover = scheduler.RetryStorageRecovery
				}
				if err := recover(ctx, "long-composite", operation); err != nil || calls != 1 || scheduler.DatabaseRetries() != 0 {
					t.Fatalf("long operation cut/replayed: %v calls=%d", err, calls)
				}
			})
		})
	}
}

func TestDatabaseDeadlineCompositeExternalDeadlineNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scheduler := &Scheduler{}
		for _, failure := range []error{context.DeadlineExceeded, errors.Join(deadlineBusyError(5), context.DeadlineExceeded), errors.Join(deadlineBusyError(5), errors.New("invariant"))} {
			calls := 0
			err := scheduler.RetryRecovery(t.Context(), "external", func(context.Context) error { calls++; return failure })
			if !errors.Is(err, failure) || calls != 1 {
				t.Fatalf("external failure replayed: %v calls=%d", err, calls)
			}
		}
	})
}

type recoveryInventory struct {
	SchedulerStore
	vmrunner.KVMJournal
}

func (recoveryInventory) PendingKVMPreflights(context.Context) ([]vmrunner.KVMExecution, error) {
	return []vmrunner.KVMExecution{{}}, nil
}

type slowRecoveryPreflight struct {
	TrustedPreflight
	delay   time.Duration
	failure error
	calls   int
}

func (p *slowRecoveryPreflight) Recover(ctx context.Context) ([]string, error) {
	p.calls++
	select {
	case <-time.After(p.delay):
		return nil, p.failure
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestDatabaseDeadlineTrustedStartupRecoveryKeepsOwnBound(t *testing.T) {
	for _, stuck := range []bool{false, true} {
		t.Run(map[bool]string{false: "slow-progress", true: "stuck-cleanup"}[stuck], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				executor := &slowRecoveryPreflight{delay: 11 * time.Second}
				if stuck {
					executor.delay = 2 * time.Minute
					executor.failure = context.DeadlineExceeded
				}
				scheduler := &Scheduler{store: recoveryInventory{}, trusted: executor}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				defer cancel()
				start := time.Now()
				if err := scheduler.recoverTrustedPreflights(deadlineScope(ctx, scheduler)); err != nil {
					t.Fatal(err)
				}
				if executor.calls != 1 || time.Since(start) != executor.delay {
					t.Fatalf("cleanup cut/replayed: calls=%d elapsed=%s", executor.calls, time.Since(start))
				}
			})
		})
	}
}

func TestDatabaseDeadlineShutdownPreservesInflightDetachedAttempt(t *testing.T) {
	for _, failure := range []error{nil, deadlineBusyError(5)} {
		synctest.Test(t, func(t *testing.T) {
			scheduler := &Scheduler{}
			owner, cancelOwner := context.WithCancel(t.Context())
			detached, cancel := context.WithTimeout(context.WithoutCancel(deadlineScope(owner, scheduler)), 5*time.Second)
			defer cancel()
			go func() { time.Sleep(time.Second); cancelOwner() }()
			calls := 0
			err := retryDatabaseError(detached, "drain-cleanup", func(ctx context.Context) error {
				calls++
				select {
				case <-time.After(3 * time.Second):
					return failure
				case <-ctx.Done():
					t.Error("shutdown aborted in-flight cleanup")
					return ctx.Err()
				}
			})
			if calls != 1 {
				t.Fatalf("shutdown replayed cleanup: %d", calls)
			}
			if failure == nil && err != nil {
				t.Fatal(err)
			}
			if failure != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("shutdown did not stop subsequent retries: %v", err)
			}
		})
	}
}

func (p *slowRecoveryPreflight) RecoverRun(ctx context.Context, _ string) error {
	p.calls++
	select {
	case <-time.After(p.delay):
		return p.failure
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestDatabaseDeadlineStuckRunCleanupReturnsWithGrantRetained(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &slowRecoveryPreflight{delay: 3 * time.Minute}
		scheduler := &Scheduler{trusted: executor}
		grant := &admissionReservation{retained: true}
		start := time.Now()
		if err := scheduler.recoverDatabasePreflight(t.Context(), "pending", grant); err != nil {
			t.Fatal(err)
		}
		if executor.calls != 1 || time.Since(start) != 2*time.Minute || !grant.retained {
			t.Fatalf("cleanup blocked/released: calls=%d elapsed=%s retained=%t", executor.calls, time.Since(start), grant.retained)
		}
	})
}

type settlementDeadlineStore struct {
	SchedulerStore
	calls int
}

func (s *settlementDeadlineStore) Run(context.Context, string) (model.Run, error) {
	return model.Run{ID: "pending", Status: model.RunRunning}, nil
}
func (s *settlementDeadlineStore) PendingRunCancellations(context.Context) ([]model.RunCancellation, error) {
	return nil, nil
}
func (s *settlementDeadlineStore) FinishRun(context.Context, string, model.RunResult) (model.Run, error) {
	s.calls++
	return model.Run{}, context.DeadlineExceeded
}
func TestDatabaseDeadlineCreateSettlerOwnsRetryBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := &settlementDeadlineStore{}
		scheduler := &Scheduler{store: db, mutationGate: func(context.Context) error { return nil }}
		ctx, cancel := context.WithTimeout(deadlineScope(t.Context(), scheduler), createSettlementAttemptBudget)
		defer cancel()
		start := time.Now()
		done, err := scheduler.settleCreateAttempt(ctx, &pendingCreate{runID: "pending", persisted: true, confirmed: true, grant: &admissionReservation{}})
		if done || !errors.Is(err, context.DeadlineExceeded) || db.calls != 1 || time.Since(start) != 0 || scheduler.DatabaseRetries() != 0 {
			t.Fatalf("settler nested retries: done=%t err=%v calls=%d elapsed=%s", done, err, db.calls, time.Since(start))
		}
	})
}

type replayDeadlineReceives struct{ calls int }

func (s *replayDeadlineReceives) RecordReceiveEvent(context.Context, model.ReceiveEventSpec) (bool, error) {
	s.calls++
	if s.calls < 3 {
		return false, context.DeadlineExceeded
	}
	return false, nil
}
func TestDatabaseDeadlineReplayRetriesStorageWithoutRepeatingGit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scheduler := &Scheduler{}
		receives := &replayDeadlineReceives{}
		git := &stubPushGit{}
		ingestor, err := NewPushIngestor(PushConfig{Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}}, Discoverer: stubRepositoryDiscoverer{}, Git: git, Receives: receives})
		if err != nil {
			t.Fatal(err)
		}
		replays := 0
		err = scheduler.RetryRecovery(t.Context(), "ReplayPending", func(ctx context.Context) error {
			replays++
			_, err := ingestor.Branch(ctx, BranchPush{EventID: "delete", Repository: "repo", Branch: "gone", Actor: "test", NewOID: strings.Repeat("0", 40)})
			return err
		})
		if err != nil || replays != 1 || git.ensureCalls != 1 || len(git.deletedBranches) != 1 || receives.calls != 3 {
			t.Fatalf("replay repeated external work: err=%v replays=%d ensure=%d deletes=%d db=%d", err, replays, git.ensureCalls, len(git.deletedBranches), receives.calls)
		}
	})
}

func TestDatabaseDeadlineTrustedRecoveryKeepsInvariantFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		invariant := errors.New("immutable binding mismatch")
		for _, failure := range []error{invariant, errors.Join(invariant, context.DeadlineExceeded), errors.Join(invariant, deadlineBusyError(5))} {
			executor := &slowRecoveryPreflight{failure: failure}
			scheduler := &Scheduler{trusted: executor}
			if err := scheduler.recoverDatabasePreflight(t.Context(), "pending", &admissionReservation{retained: true}); !errors.Is(err, invariant) || executor.calls != 1 {
				t.Fatalf("recovery hid/replayed invariant: %v calls=%d", err, executor.calls)
			}
		}
	})
}

func TestDatabaseDeadlineCompositeCallerBudgetBoundsRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scheduler := &Scheduler{}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		start := time.Now()
		calls := 0
		err := scheduler.RetryRecovery(ctx, "contention", func(context.Context) error { calls++; return deadlineBusyError(5) })
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != time.Minute || calls < 2 {
			t.Fatalf("composite retry ignored caller budget: %v elapsed=%s calls=%d", err, time.Since(start), calls)
		}
	})
}
