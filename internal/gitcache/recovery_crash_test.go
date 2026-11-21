package gitcache_test

import (
	"context"
	"encoding/json"

	"io"

	"os"

	"path/filepath"

	"strings"

	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReviewSecondFetchCrash(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if e := c.Init(t.Context(), gittest.Repository); e != nil {
		t.Fatal(e)
	}
	f.Write(t, "next", []byte("next"))
	next := f.CommitFile(t, "next")
	f.Git(t, f.Work, "push", f.Bare, "main")
	reviewFault(t, f, map[string]any{"args": []string{"update-ref", "--no-deref"}, "stop": true})
	done := make(chan error, 1)
	go func() { done <- c.ForceFetch(t.Context(), gittest.Repository) }()
	reviewKill(t, f)
	t.Log("interrupted fetch:", <-done)
	reviewDisarm(t, f)
	_ = c.Close()
	c = reviewNew(t, cfg)
	if e := c.ForceFetch(t.Context(), gittest.Repository); e != nil {
		t.Fatal(e)
	}
	got := f.Git(t, f.Store(), "rev-parse", "refs/heads/main")
	tracking := f.Git(t, f.Store(), "rev-parse", "refs/oberth/upstream/heads/main")
	t.Logf("old=%s new=%s public=%s tracking=%s", f.Commit, next, got, tracking)
	if got != next {
		t.Error("successful retry permanently leaves public mirror at old commit")
	}
}
func TestReviewSecondKeepCrash(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if e := c.Init(t.Context(), gittest.Repository); e != nil {
		t.Fatal(e)
	}
	reviewFault(t, f, map[string]any{"args": []string{"update-ref"}, "needle": "refs/oberth/keep/run/", "phase": "before"})
	done := make(chan error, 1)
	go func() { done <- c.Keep(t.Context(), "run", f.Bindings(f.Commit)) }()
	reviewKill(t, f)
	t.Log("interrupted keep:", <-done)
	reviewDisarm(t, f)
	_ = c.Close()
	c = reviewNew(t, cfg)
	locks := reviewLocks(t, f)
	t.Logf("locks after reopen: %v", locks)
	f.Git(t, f.Store(), "fsck", "--full")
	if e := c.Keep(t.Context(), "run", f.Bindings(f.Commit)); e != nil {
		t.Errorf("idempotent retry failed after reopen: %v", e)
	}
	if len(locks) > 0 {
		t.Error("dangling native ref lock survived restart")
	}
}
func TestReviewSecondReceiveCrash(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			f := gittest.New(t)
			cfg := reviewConfig(t, f)
			cfg.Binary = reviewWrapper(t, f)
			c := reviewNew(t, cfg)
			if e := c.Init(t.Context(), gittest.Repository); e != nil {
				t.Fatal(e)
			}
			f.Write(t, "next", []byte("next"))
			next := f.CommitFile(t, "next")
			reviewFault(t, f, map[string]any{"args": []string{"receive-pack"}, "needle": "refs/heads/topic", "phase": phase})
			delivered := 0
			var saved gitcache.ReceiveEvent
			deliver := func(_ context.Context, e gitcache.ReceiveEvent) ([]string, error) {
				delivered++
				saved = e
				return nil, nil
			}
			done := make(chan error, 1)
			go func() {
				done <- c.Receive(
					t.Context(),
					gitcache.ReceiveRequest{
						Repository:  gittest.Repository,
						Actor:       "a",
						Fingerprint: "f",
						Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
						Input:       receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", NewSHA: next}),
						Output:      io.Discard,
						Deliver:     deliver,
					},
				)
			}()
			reviewKill(t, f)
			t.Log("interrupted receive:", <-done)
			reviewDisarm(t, f)
			_ = c.Close()
			c = reviewNew(t, cfg)
			for i := 0; i < 2; i++ {
				if e := c.ReplayPending(t.Context(), deliver); e != nil {
					t.Errorf("replay %d: %v", i, e)
				}
			}
			f.Git(t, f.Store(), "fsck", "--full")
			t.Logf("delivered=%d updates=%v locks=%v", delivered, saved.Updates, reviewLocks(t, f))
			if phase == "after" && (delivered != 1 || len(saved.Updates) != 1 || saved.Updates[0].NewSHA != next) {
				t.Error("accepted update not replayed exactly once")
			}
			if len(reviewLocks(t, f)) > 0 {
				t.Error("receive ref locks survive restart")
			}
		})
	}
}
func TestReviewSecondPublishCrash(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			f := gittest.New(t)
			cfg := reviewConfig(t, f)
			cfg.Binary = reviewWrapper(t, f)
			c := reviewNew(t, cfg)
			if e := c.Init(t.Context(), gittest.Repository); e != nil {
				t.Fatal(e)
			}
			f.Write(t, "next", []byte("next"))
			next := f.CommitFile(t, "next")
			f.Git(t, f.Store(), "fetch", f.Work, "refs/heads/main:refs/heads/topic")
			reviewFault(t, f, map[string]any{"args": []string{"push"}, "needle": "refs/heads/topic", "phase": phase})
			req := publicationFor(f, "refs/heads/topic", next, "")
			done := make(chan error, 1)
			go func() { _, e := c.Publish(t.Context(), req); done <- e }()
			reviewKill(t, f)
			t.Log("interrupted publication:", <-done)
			reviewDisarm(t, f)
			_ = c.Close()
			c = reviewNew(t, cfg)
			_, e := c.Publish(t.Context(), req)
			t.Log("retry:", e)
			if phase == "after" && e != nil {
				t.Error("accepted publication not idempotent", e)
			}
			// The remote store is not owned by C6; its abandoned pre-commit lock is reported, not repaired here.
			f.Git(t, f.Bare, "fsck", "--full")
		})
	}
}
func TestReviewSecondKeepAfterCommit(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if e := c.Init(t.Context(), gittest.Repository); e != nil {
		t.Fatal(e)
	}
	reviewFault(t, f, map[string]any{"args": []string{"update-ref"}, "needle": "refs/oberth/keep/run/", "phase": "after"})
	done := make(chan error, 1)
	go func() { done <- c.Keep(t.Context(), "run", f.Bindings(f.Commit)) }()
	reviewKill(t, f)
	t.Log("interrupted keep:", <-done)
	reviewDisarm(t, f)
	_ = c.Close()
	c = reviewNew(t, cfg)
	if e := c.Keep(t.Context(), "run", f.Bindings(f.Commit)); e != nil {
		t.Fatal(e)
	}
	if len(reviewLocks(t, f)) != 0 {
		t.Fatal("lock residue")
	}
	f.Git(t, f.Store(), "fsck", "--full")
}

func TestReviewSecondReceiveQuarantineCrash(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	entered := make(chan struct{})
	cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	c := reviewNew(t, cfg)
	if e := c.Init(t.Context(), gittest.Repository); e != nil {
		t.Fatal(e)
	}
	f.Write(t, "next", []byte("next"))
	next := f.CommitFile(t, "next")
	input := receiveBatchInput(
		t,
		f,
		[]gitcache.RefUpdate{{Ref: "refs/heads/topic", NewSHA: next}},
		[]string{"oberth.run=1"},
	)
	done := make(chan error, 1)
	go func() {
		done <- c.Receive(
			t.Context(),
			gitcache.ReceiveRequest{
				Repository:  gittest.Repository,
				Actor:       "a",
				Fingerprint: "f",
				Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
				Input:       input,
				Output:      io.Discard,
			},
		)
	}()
	<-entered
	pid := reviewNativePID(t, f, "receive-pack")
	var e error
	if e = syscall.Kill(pid, syscall.SIGKILL); e != nil {
		t.Fatal(e)
	}
	t.Log("kill during quarantine:", <-done)
	_ = c.Close()
	c = reviewNew(t, cfg)
	if e = c.ReplayPending(t.Context(), nil); e != nil {
		t.Log("replay:", e)
	}
	if e = c.GC(t.Context()); e != nil {
		t.Log("gc:", e)
	}
	debris, e := filepath.Glob(filepath.Join(f.Store(), "objects", "tmp_objdir-incoming-*"))
	if e != nil {
		t.Fatal(e)
	}
	t.Log("quarantine debris:", debris)
	if len(debris) > 0 {
		t.Error("interrupted native quarantines retained after reopen and GC")
	}
}

func TestClonePublishesTrackingAndDefaultBeforeCanonicalRename(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	reviewFault(t, f, map[string]any{"args": []string{"update-ref"}, "stop": true})
	done := make(chan error, 1)
	go func() { done <- c.Init(t.Context(), gittest.Repository) }()
	reviewKill(t, f)
	if err := <-done; err == nil {
		t.Fatal("interrupted init succeeded")
	}
	reviewDisarm(t, f)
	if _, err := os.Stat(filepath.Join(f.Store(), "HEAD")); !os.IsNotExist(err) {
		t.Fatal("incomplete clone was published", err)
	}
	_ = c.Close()
	c = reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal("clone retry", err)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/oberth/upstream/heads/main"); got != f.Commit {
		t.Fatal("tracking not initialized", got)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/main"); got != f.Commit {
		t.Fatal("default not initialized", got)
	}
}

func reviewNativePID(t *testing.T, f *gittest.Fixture, command string) int {
	t.Helper()
	log, e := os.ReadFile(filepath.Join(f.Root, "commands.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	pid := 0
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		var r struct {
			PID  int      `json:"pid"`
			Args []string `json:"argv"`
		}
		if e = json.Unmarshal([]byte(line), &r); e != nil {
			t.Fatal(e)
		}
		for _, a := range r.Args {
			if a == command {
				pid = r.PID
			}
		}
	}
	if pid == 0 {
		t.Fatal("no receive PID")
	}
	return pid
}
