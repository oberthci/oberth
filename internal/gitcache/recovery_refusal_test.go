package gitcache_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceiveRecordRejectsDifferentCanonicalRepository(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	r := gateReceive(f)
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", NewSHA: f.Commit})
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, io.ErrClosedPipe }
	if err := c.Receive(t.Context(), r); err == nil {
		t.Fatal("save did not fail")
	}
	record, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil {
		t.Fatal(err)
	}
	record.Event.Repository = "origin/team/other"
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.Store(), "oberth-receive.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	quarantined, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil || quarantined.State != "corrupt" || quarantined.Event.Repository != gittest.Repository {
		t.Fatal("cross-repository record was not exposed as quarantined evidence", quarantined, err)
	}
	if err = c.RetryReceive(t.Context(), gittest.Repository, r.Deliver); err == nil {
		t.Fatal("cross-repository receive record accepted")
	}
}
func TestReceiveRecoveryRejectsUnknownRefMovement(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Write(t, "next", []byte("next"))
	next := f.CommitFile(t, "next")
	c := receiveCache(t, f, nil)
	r := gateReceive(f)
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", NewSHA: next})
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, io.ErrClosedPipe }
	if err := c.Receive(t.Context(), r); err == nil {
		t.Fatal("save did not fail")
	}
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	called := false
	err := c.ReplayPending(
		t.Context(),
		func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
			called = true
			return nil, nil
		},
	)
	if err == nil || called {
		t.Fatal("unexplained ref movement replayed", err)
	}
}
func TestActiveUploadRootsSurvivePublicDeletionAndGC(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil })
	input, write := io.Pipe()
	defer func() { _ = write.Close() }()
	begin := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.Upload(
			t.Context(),
			gittest.Repository,
			gitcache.ServiceIO{
				Input:  input,
				Output: io.Discard,
				Begin: func() {
					close(begin)
				},
			},
		)
	}()
	<-begin
	for _, ref := range []string{"refs/heads/main", "refs/oberth/upstream/heads/main"} {
		f.Git(t, f.Store(), "update-ref", "-d", ref)
	}
	if err := c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs == "" {
		t.Fatal("active upload roots released")
	}
	_, _ = io.WriteString(write, "0000")
	_ = write.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGCLeavesActiveNativeReceiveUntouched(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	input, send := io.Pipe()
	defer func() { _ = send.Close() }()
	begin := make(chan struct{})
	done := make(chan error, 1)
	r := gateReceive(f)
	r.Input = input
	r.Output = io.Discard
	r.Begin = func() { close(begin) }
	go func() { done <- c.Receive(t.Context(), r) }()
	<-begin
	if err := c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.Root, "commands.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"gc"`) {
		t.Error("GC touched the active receive repository")
	}
	_, _ = io.WriteString(send, "0000")
	_ = send.Close()
	if err := <-done; err != nil {
		t.Fatal("empty handshake failed", err)
	}
}
