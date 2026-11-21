package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceiveStaleOldNeverFencesRepository(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "alone"
		if mixed {
			name = "with-valid-command"
		}
		t.Run(name, func(t *testing.T) { checkStaleReceive(t, mixed) })
	}
}

func checkStaleReceive(t *testing.T, mixed bool) {
	t.Helper()
	f := gittest.New(t)
	f.RequireBatchedReceive(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/stale", f.Commit)
	f.Write(t, "stale.txt", []byte("next"))
	next := f.CommitFile(t, "stale")
	updates := []gitcache.RefUpdate{{Ref: "refs/heads/stale", OldSHA: next, NewSHA: next}}
	if mixed {
		updates = append(updates, gitcache.RefUpdate{Ref: "refs/heads/valid", NewSHA: next})
	}
	request := fourthAgentPush(t, f, updates, nil)
	var status bytes.Buffer
	request.Output = &status
	var events []gitcache.ReceiveEvent
	request.Deliver = func(_ context.Context, e gitcache.ReceiveEvent) ([]string, error) {
		events = append(events, e)
		return nil, nil
	}
	if err := c.Receive(t.Context(), request); err != nil {
		t.Fatal("stale old fenced receive", err)
	}
	if !strings.Contains(status.String(), "ng refs/heads/stale") {
		t.Fatal("probe did not reach native stale-old refusal", status.String())
	}
	want := 0
	if mixed {
		want = 1
	}
	if len(events) != want {
		t.Fatal("wrong event count", events)
	}
	if mixed && (len(events[0].Updates) != 1 || events[0].Updates[0].Ref != "refs/heads/valid") {
		t.Fatal("refused command delivered", events)
	}
	assertStaleReceiveSettled(t, c, f)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = reviewNew(t, cfg)
	assertStaleReceiveSettled(t, c, f)
	checkPushAndAbandonAfterStale(t, c, f, next)
}

func checkPushAndAbandonAfterStale(t *testing.T, c *gitcache.Cache, f *gittest.Fixture, next string) {
	t.Helper()
	request := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/stale", OldSHA: f.Commit, NewSHA: next}}, nil)
	request.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, io.ErrClosedPipe }
	if err := c.Receive(t.Context(), request); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("ordinary push blocked", err)
	}
	pending, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil {
		t.Fatal("pending view blocked", err)
	}
	if err = c.AbandonReceive(t.Context(), gittest.Repository, pending.Event.ID); err != nil {
		t.Fatal("abandon blocked", err)
	}
	if completionState(t, f) != "absent" {
		t.Fatal("abandon did not settle")
	}
}

func assertStaleReceiveSettled(t *testing.T, c *gitcache.Cache, f *gittest.Fixture) {
	t.Helper()
	if completionState(t, f) != "absent" {
		t.Fatal("stale command retained a record")
	}
	ref, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/stale")
	if err != nil || ref.ObjectSHA != f.Commit {
		t.Fatal("stale command blocked read or changed ref", ref, err)
	}
	if _, err = c.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
		t.Fatal("pending view fenced", err)
	}
	if err = c.RetryReceive(t.Context(), gittest.Repository, func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("settled receive replayed")
		return nil, nil
	}); err != nil {
		t.Fatal("retry fenced", err)
	}
}
