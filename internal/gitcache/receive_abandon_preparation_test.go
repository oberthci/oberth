package gitcache_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestAbandonKeepsOnlyPreparedAppliedReferences(t *testing.T) {
	for _, damage := range []string{"between", "bypassed"} {
		t.Run(damage, func(t *testing.T) { checkAbandonPreparation(t, damage) })
	}
}

func checkAbandonPreparation(t *testing.T, damage string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	for _, ref := range []string{"topic", "second"} {
		f.Git(t, f.Store(), "update-ref", "refs/heads/"+ref, f.Commit)
	}
	f.Write(t, "abandon-prepared.txt", []byte("source"))
	next := f.CommitFile(t, "abandon prepared")
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: f.Commit}, {Ref: "refs/heads/second", OldSHA: f.Commit, NewSHA: next}}, nil)
	var once sync.Once
	r.Begin = func() {
		once.Do(func() { installPreparationStop(t, preparationStop{Store: f.Store(), Damage: damage}, 0) })
	}
	deferAbandonRecovery(t, c, r)
	pending, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.AbandonReceive(t.Context(), gittest.Repository, pending.Event.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/second"); got != f.Commit {
		t.Fatal("abandon kept an unprepared write", got)
	}
	want := ""
	if damage == "bypassed" {
		want = f.Commit
	}
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(objectname)", "refs/heads/topic"); got != want {
		t.Fatal("abandon ignored deletion preparation", got, want)
	}
	retryCompletion(t, c, func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("abandoned receive delivered")
		return nil, nil
	})
	if completionState(t, f) != "absent" {
		t.Fatal("abandon did not remove record")
	}
}

func deferAbandonRecovery(t *testing.T, c *gitcache.Cache, r gitcache.ReceiveRequest) {
	t.Helper()
	status := &completionStatus{failure: io.ErrClosedPipe}
	r.Output = status
	gitcache.SetReceiveCleanupBudget(c, time.Millisecond)
	var release func()
	t.Cleanup(func() {
		if release != nil {
			release()
		}
	})
	gitcache.AfterReceiveTransport(c, func() {
		if status.seen.Load() {
			var err error
			release, err = gitcache.HoldReceiveRepository(t.Context(), c, gittest.Repository)
			if err != nil {
				t.Error(err)
			}
		}
	})
	if err := c.Receive(t.Context(), r); !errors.Is(err, context.DeadlineExceeded) || release == nil {
		t.Fatal("cleanup did not stop before recovery", err)
	}
	release()
	release = nil
}
