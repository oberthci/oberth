package cronsync_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/cronsync"
)

func TestIrrelevantRefsAndCanceledWaitDoNotWaitOrList(t *testing.T) {
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
	finished := make(chan error, 1)
	go func() { finished <- f.c.SyncAll(t.Context()) }()
	<-entered
	for _, refs := range [][]string{{"refs/tags/v1"}, {"refs/heads/unrelated"}} {
		if err := f.c.RefsChanged(t.Context(), f.repo.Qualified, refs); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.c.SetPause(ctx, f.repo, "nightly"); !errors.Is(err, context.Canceled) {
		t.Fatal("lock wait ignored cancellation", err)
	}
	once.Do(func() { close(release) })
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestBlockedGitAndNativeCallsPreserveCanceledWork(t *testing.T) {
	for _, port := range []string{"git", "native"} {
		t.Run(port, func(t *testing.T) {
			f := newFixture(t)
			entered := make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hook := func(ctx context.Context, verb, _ string) error {
				if verb == "resolve" || verb == "list" {
					close(entered)
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			if port == "git" {
				f.gitHook = hook
			} else {
				f.hook = hook
			}
			finished := make(chan error, 1)
			go func() { finished <- f.c.SyncAll(ctx) }()
			<-entered
			cancel()
			if err := <-finished; !errors.Is(err, context.Canceled) {
				t.Fatal("canceled work lost", err)
			}
		})
	}
}

func TestIrrelevantRefsSkipNativeIOAndAdmission(t *testing.T) {
	f := newFixture(t)
	f.sync()
	calls := len(f.calls)
	nativeReads := 0
	f.hook = func(context.Context, string, string) error { nativeReads++; return nil }
	for _, ref := range []string{"refs/heads/unrelated", "refs/tags/v1"} {
		if err := f.c.RefsChanged(t.Context(), f.repo.Qualified, []string{ref}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.calls) != calls || nativeReads != 0 {
		t.Fatal("irrelevant refs reached native storage or admission", len(f.calls), nativeReads)
	}
}
