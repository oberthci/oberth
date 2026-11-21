package gitcache_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestUnpreparedRefShapeChangeRestoresBefore(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, mode := range []string{"bypassed-stop", "empty-stop", "empty-live"} {
			name := "parent-to-child/" + mode
			if reverse {
				name = "child-to-parent/" + mode
			}
			t.Run(name, func(t *testing.T) { checkUnpreparedRefShape(t, reverse, mode) })
		}
	}
}

func checkUnpreparedRefShape(t *testing.T, reverse bool, mode string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	before, after := "refs/heads/topic", "refs/heads/topic/leaf"
	if reverse {
		before, after = after, before
	}
	f.Git(t, f.Store(), "update-ref", before, f.Commit)
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: before, OldSHA: f.Commit}, {Ref: after, NewSHA: f.Commit}}, nil)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("unprepared shape change delivered")
		return nil, nil
	}
	if strings.HasSuffix(mode, "stop") {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		stopUnpreparedRefShape(t, preparationStop{Root: cfg.Root, Scratch: cfg.Scratch, Binary: cfg.Binary,
			Hook: cfg.HookBinary, Store: f.Store(), Commit: f.Commit, Boundary: "transport", Damage: strings.Split(mode, "-")[0]}, r.Input)
		if got := f.Git(t, f.Store(), "rev-parse", after); got != f.Commit {
			t.Fatal("native shape change did not commit", got)
		}
		c = reopenStoppedReceive(t, cfg)
	} else {
		receiveEmptyRefShape(t, c, f, r)
	}
	ref, err := c.ResolveRef(t.Context(), gittest.Repository, before)
	if err != nil || ref.CommitSHA != f.Commit || completionState(t, f) != "absent" {
		t.Fatal("shape rollback fenced the repository", ref, err)
	}
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", after); got != "" && got != before {
		t.Fatal("unprepared replacement survived", got)
	}
	retryCompletion(t, c, r.Deliver)
	ordinary := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/after-swap", NewSHA: f.Commit}}, nil)
	calls := 0
	ordinary.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { calls++; return nil, nil }
	if err = c.Receive(t.Context(), ordinary); err != nil || calls != 1 {
		t.Fatal("ordinary push failed after rollback", err, calls)
	}
}

func stopUnpreparedRefShape(t *testing.T, shape preparationStop, input io.ReadCloser) {
	t.Helper()
	data, err := io.ReadAll(input)
	_ = input.Close()
	if err != nil {
		t.Fatal(err)
	}
	shape.Pack = filepath.Join(filepath.Dir(shape.Scratch), "shape.pack")
	if err = os.WriteFile(shape.Pack, data, 0600); err != nil {
		t.Fatal(err)
	}
	runPreparationStop(t, shape)
}

func receiveEmptyRefShape(t *testing.T, c *gitcache.Cache, f *gittest.Fixture, r gitcache.ReceiveRequest) {
	t.Helper()
	var once sync.Once
	r.Begin = func() {
		once.Do(func() { installPreparationStop(t, preparationStop{Store: f.Store(), Damage: "empty"}, 0) })
	}
	var status, feedback bytes.Buffer
	r.Output, r.Diagnostics = &status, &feedback
	if err := c.Receive(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"refs/heads/topic", "refs/heads/topic/leaf"} {
		if !strings.Contains(status.String(), "ok "+ref) || !strings.Contains(feedback.String(), "Reference: "+ref+".") {
			t.Fatal("unprepared shape lacks its corrected native status", status.String(), feedback.String())
		}
	}
}
