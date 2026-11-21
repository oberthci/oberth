package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestRefusedReceiveSurvivesFailedRejectionAndRollback(t *testing.T) {
	for _, abandon := range []bool{false, true} {
		name := "retry"
		if abandon {
			name = "abandon"
		}
		t.Run(name, func(t *testing.T) { checkRefusedStorageRecovery(t, abandon) })
	}
}

func checkRefusedStorageRecovery(t *testing.T, abandon bool) {
	t.Helper()
	f, c, next := refuseWithBrokenStorage(t)
	deliver := func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("refused event delivered")
		return nil, nil
	}
	if err := os.Chmod(f.Store(), 0700); err != nil {
		t.Fatal(err)
	}
	pending, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil || pending.State != "rejected" {
		t.Fatal("refusal forgotten by pending view", pending, err)
	}
	checkRefusedReceiveBlocked(t, c, f, next)
	if err = c.RetryReceive(t.Context(), gittest.Repository, deliver); err == nil {
		t.Fatal("refused receive delivered while blocked", err)
	}
	if err = c.AbandonReceive(t.Context(), gittest.Repository, pending.Event.ID); err == nil {
		t.Fatal("abandon bypassed blocked rollback")
	}
	finishRefusedStorageRecovery(t, f, func() error {
		if abandon {
			return c.AbandonReceive(t.Context(), gittest.Repository, pending.Event.ID)
		}
		return c.RetryReceive(t.Context(), gittest.Repository, deliver)
	})
}

func refuseWithBrokenStorage(t *testing.T) (*gittest.Fixture, *gitcache.Cache, string) {
	t.Helper()
	f, c, next := completionFixture(t)
	f.Git(t, f.Store(), "update-ref", "refs/heads/second", f.Commit)
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: f.Commit}, {Ref: "refs/heads/second", OldSHA: f.Commit, NewSHA: next}}, nil)
	var ends atomic.Int32
	foreign := filepath.Join(f.Root, "foreign-lock")
	if err := os.WriteFile(foreign, []byte("another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(f.Store(), "refs", "heads", "topic.lock")
	t.Cleanup(func() { _ = os.Chmod(f.Store(), 0700) })
	gitcache.BeforeReceivePreparation(c, func() {
		if ends.Add(1) == 2 {
			if err := os.Link(foreign, lock); err != nil {
				t.Error(err)
			}
			if err := os.Chmod(f.Store(), 0500); err != nil {
				t.Error(err)
			}
		}
	})
	var feedback bytes.Buffer
	r.Diagnostics = &feedback
	calls := 0
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { calls++; return nil, nil }
	err := c.Receive(t.Context(), r)
	if calls != 0 || !errors.Is(err, os.ErrPermission) || !strings.Contains(feedback.String(), "push refused; rollback is blocked.") {
		t.Fatal("fault did not refuse preparation and rollback", err, feedback.String())
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("rollback obstruction disappeared", err)
	}
	if record := readCompletionRecord(t, f); record.State != "ready" || len(record.Prepared) != 1 {
		t.Fatal("probe did not leave the first prepared transaction", record)
	}
	return f, c, next
}

func finishRefusedStorageRecovery(t *testing.T, f *gittest.Fixture, finish func() error) {
	t.Helper()
	lock := filepath.Join(f.Store(), "refs", "heads", "topic.lock")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != nil || completionState(t, f) != "absent" {
		t.Fatal("refusal did not settle after repair", err)
	}
	for _, ref := range []string{"topic", "second"} {
		if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/"+ref); got != f.Commit {
			t.Fatal("refusal left an applied reference", ref, got)
		}
	}
}

func checkRefusedReceiveBlocked(t *testing.T, c *gitcache.Cache, f *gittest.Fixture, next string) {
	t.Helper()
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/topic", NewSHA: next}}, nil)
	var feedback bytes.Buffer
	r.Diagnostics = &feedback
	err := c.Receive(t.Context(), r)
	assertReceiveRollbackFeedback(t, err, feedback.String())
	if strings.Contains(feedback.String(), f.Store()) {
		t.Fatal("blocked push exposed native path")
	}
}

func TestAbandonFirstUsesRefusalThatCouldNotBeWritten(t *testing.T) {
	f, c, _ := refuseWithBrokenStorage(t)
	record := readCompletionRecord(t, f)
	if err := os.Chmod(f.Store(), 0700); err != nil {
		t.Fatal(err)
	}
	finishRefusedStorageRecovery(t, f, func() error {
		// No ordinary operation may persist the refusal before this repair.
		if current := readCompletionRecord(t, f); current.State != "ready" {
			t.Fatal("refusal was already persisted", current.State)
		}
		return c.AbandonReceive(t.Context(), gittest.Repository, record.Event.ID)
	})
	retryCompletion(t, c, func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("abandoned refusal was delivered")
		return nil, nil
	})
}
