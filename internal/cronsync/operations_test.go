package cronsync_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/cronsync"
)

func TestEachCallerRunsItsOwnSyncAndReceivesItsOwnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		outage := errors.New("admission unavailable")
		calls := 0
		f.admitHook = func(ctx context.Context, _ cronsync.Preparation) error {
			calls++
			if calls != 1 {
				return nil
			}
			close(entered)
			select {
			case <-release:
				return outage
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		first, second := make(chan error, 1), make(chan error, 1)
		go func() { first <- f.c.SyncAll(t.Context()) }()
		<-entered
		go func() { second <- f.c.SyncRepository(t.Context(), f.repo.Qualified) }()
		synctest.Wait()
		if calls != 1 {
			t.Fatal("operations overlapped")
		}
		once.Do(func() { close(release) })
		if err := <-first; !errors.Is(err, outage) {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Fatal("inherited another caller's failure", err)
		}
		if calls != 2 {
			t.Fatal("a caller did not perform its operation", calls)
		}
	})
}

func TestRefChangesUseTheCallersDeadline(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	deadline, _ := ctx.Deadline()
	calls := 0
	hook := func(ctx context.Context, _, _ string) error {
		got, present := ctx.Deadline()
		if !present || !got.Equal(deadline) {
			t.Fatal("caller deadline changed", got, deadline)
		}
		calls++
		return nil
	}
	f.gitHook, f.hook = hook, hook
	if err := f.c.RefsChanged(ctx, f.repo.Qualified, []string{"refs/heads/main"}); err != nil || calls == 0 {
		t.Fatal(calls, err)
	}
}

func TestRestartReadsAuditedPauseBeforeWriting(t *testing.T) {
	f := newFixture(t)
	f.sync()
	f.paused = true
	restarted, err := cronsync.New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.SyncAll(t.Context()); err != nil || !f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("restart lost audited pause", err)
	}
	f.paused = false
	if err := restarted.SetPause(t.Context(), f.repo, "nightly"); err != nil || f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("resume ignored audited intent", err)
	}
}

func TestWaitingOperationCanBeCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		f.admitHook = func(ctx context.Context, _ cronsync.Preparation) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		owner, waiter := make(chan error, 1), make(chan error, 1)
		go func() { owner <- f.c.SyncAll(t.Context()) }()
		<-entered
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { waiter <- f.c.SyncRepository(ctx, f.repo.Qualified) }()
		synctest.Wait()
		cancel()
		if err := <-waiter; !errors.Is(err, context.Canceled) {
			t.Fatal("wait ignored caller cancellation", err)
		}
		once.Do(func() { close(release) })
		if err := <-owner; err != nil {
			t.Fatal(err)
		}
	})
}
