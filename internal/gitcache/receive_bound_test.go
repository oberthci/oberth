package gitcache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceiveCleanupDeadlinePreservesDecision(t *testing.T) {
	for _, damage := range []string{"intact", "missing"} {
		for _, unchanged := range []bool{false, true} {
			name := damage + "/changed"
			if unchanged {
				name = damage + "/unchanged"
			}
			t.Run(name, func(t *testing.T) { checkCleanupDeadline(t, damage, unchanged) })
		}
	}
}

func checkCleanupDeadline(t *testing.T, damage string, unchanged bool) {
	t.Helper()
	f, c, next := completionFixture(t)
	if unchanged {
		next = f.Commit
	}
	request := completionRequest(t, f, next)
	status := &completionStatus{failure: errors.New("fixture transport disconnect")}
	request.Output = status
	gitcache.SetReceiveCleanupBudget(c, time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var once sync.Once
	request.Begin = func() { once.Do(func() { damageCompletionHook(t, f, damage) }) }
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	gitcache.AfterReceiveTransport(c, func() {
		if status.seen.Load() {
			var err error
			release, err = gitcache.HoldReceiveRepository(ctx, c, gittest.Repository)
			if err != nil {
				t.Error("hold repository", err)
			}
		}
	})
	delivered := 0
	request.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	err := c.Receive(ctx, request)
	if release == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cleanup did not reach injected deadline", err)
	}
	wantState := "rejected"
	accepted := damage == "intact"
	if accepted {
		wantState = "ready"
	}
	if got := completionState(t, f); got != wantState {
		t.Errorf("decision before lock release: got %s, want %s", got, wantState)
	}
	release()
	release = nil
	retryCompletion(t, c, request.Deliver)
	assertCompletedPush(t, c, f, completionExpectation{next: next, accepted: accepted && !unchanged, delivered: &delivered, deliver: request.Deliver})
}

func TestReceiveLaterPreparationRefusalRollsBackWholePush(t *testing.T) {
	f, c, next := completionFixture(t)
	f.Git(t, f.Store(), "update-ref", "refs/heads/second", f.Commit)
	request := completionRequest(t, f, next)
	// Native receive commits deletions before its batch of writes.
	request.Input = receiveBatchInput(t, f, []gitcache.RefUpdate{
		{Ref: "refs/heads/topic", OldSHA: f.Commit},
		{Ref: "refs/heads/second", OldSHA: f.Commit, NewSHA: next},
	}, nil)
	var ends atomic.Int32
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	gitcache.BeforeReceivePreparation(c, func() {
		if ends.Add(1) == 2 {
			if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(objectname)", "refs/heads/topic"); got != "" {
				t.Error("later refusal did not follow first ref commit", got)
			}
			release = gitcache.HoldReceivePublication(c, gittest.Repository, "refs/heads/second")
		}
	})
	delivered := 0
	request.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := c.Receive(ctx, request); err == nil || release == nil {
		t.Fatal("later preparation was not refused", err, "gate calls", ends.Load())
	}
	assertCompletedPush(t, c, f, completionExpectation{next: next, delivered: &delivered, deliver: request.Deliver})
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/second"); got != f.Commit {
		t.Error("refused second ref moved", got)
	}
}
