package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestCorruptReceiveCanBeAbandoned(t *testing.T) {
	for _, damage := range []string{"truncated", "lost-predecessors"} {
		t.Run(damage, func(t *testing.T) { checkCorruptAbandon(t, damage) })
	}
}

func checkCorruptAbandon(t *testing.T, damage string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	original, next := pendingAbandonReceive(t, c, f)
	path := filepath.Join(f.Store(), "oberth-receive.json")
	data := truncateReceiveRecord(t, path, damage)
	wantRef, wantID := f.Commit, original.Event.ID
	if damage == "lost-predecessors" {
		wantRef, wantID = next, ""
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = reviewNew(t, cfg)
	pending := inspectCorruptReceive(t, c, wantID)
	if err := c.AbandonReceive(t.Context(), gittest.Repository, "another-event"); err == nil {
		t.Fatal("a different identity abandoned this record")
	}
	if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, data) {
		t.Fatal("identity refusal changed the record", err)
	}
	if err := c.AbandonReceive(t.Context(), gittest.Repository, pending.Event.ID); err != nil {
		t.Fatal("corrupt receive could not be abandoned", err)
	}
	if completionState(t, f) != "absent" || f.Git(t, f.Store(), "rev-parse", "refs/heads/topic") != wantRef {
		t.Fatal("abandon did not settle the record and its available predecessor evidence")
	}
	if damage == "truncated" && f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/receive-"+original.Event.ID+"/") != "" {
		t.Fatal("abandon retained the known receive owner")
	}
	pushAfterAbandon(t, c, f, wantRef)
}

func inspectCorruptReceive(t *testing.T, c *gitcache.Cache, expected string) gitcache.Reservation {
	t.Helper()
	pending, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil || pending.State != "corrupt" || pending.Event.ID == "" {
		t.Fatal("corrupt record has no inspectable identity", pending, err)
	}
	if expected != "" && pending.Event.ID != expected {
		t.Fatal("corruption changed a recoverable event identity")
	}
	calls := 0
	err = c.RetryReceive(t.Context(), gittest.Repository, func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		calls++
		return nil, nil
	})
	var corrupt *gitcache.CorruptReceiveError
	if !errors.As(err, &corrupt) || calls != 0 {
		t.Fatal("corrupt receive was delivered", err, calls)
	}
	return pending
}

func pushAfterAbandon(t *testing.T, c *gitcache.Cache, f *gittest.Fixture, old string) {
	t.Helper()
	f.Write(t, "abandon.txt", []byte("ordinary source"))
	next := f.CommitFile(t, "ordinary")
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/topic", OldSHA: old, NewSHA: next}}, nil)
	calls := 0
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { calls++; return nil, nil }
	if err := c.Receive(t.Context(), r); err != nil || calls != 1 {
		t.Fatal("abandoned record blocked an ordinary push", err, calls)
	}
	if completionState(t, f) != "absent" || f.Git(t, f.Store(), "rev-parse", "refs/heads/topic") != next {
		t.Fatal("ordinary push was not settled")
	}
	if err := c.GC(t.Context()); err != nil {
		t.Fatal("abandon blocked maintenance", err)
	}
	if roots := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); roots != "" {
		t.Fatal("abandoned ownership survived collection", roots)
	}
}

func pendingAbandonReceive(t *testing.T, c *gitcache.Cache, f *gittest.Fixture) (gitcache.Reservation, string) {
	t.Helper()
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	f.Write(t, "abandon.txt", []byte("pending source"))
	next := f.CommitFile(t, "pending")
	r := completionRequest(t, f, next)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, io.ErrClosedPipe }
	if err := c.Receive(t.Context(), r); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("receive was not left pending", err)
	}
	original, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil {
		t.Fatal(err)
	}
	return original, next
}

func truncateReceiveRecord(t *testing.T, path, damage string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = data[:len(data)-1]
	if damage == "lost-predecessors" {
		data = []byte(`{"prepared":true,"before":{`)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}
