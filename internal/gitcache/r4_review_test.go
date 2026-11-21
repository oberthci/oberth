package gitcache_test

// Third-pass review probes, batch A. Each test asserts the behaviour a correct
// component must have; a failure is a finding, a pass is evidence.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func thirdInit(t *testing.T, cfg gitcache.Config) *gitcache.Cache {
	t.Helper()
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal("init:", err)
	}
	return c
}

func thirdServes(ctx context.Context, c *gitcache.Cache, f *gittest.Fixture) error {
	_, err := c.ReadFile(ctx, gitport.Revision{Repository: gittest.Repository, Commit: f.Commit}, "file.txt")
	return err
}

// An upstream that replaces branch a/b by branch a (or the reverse) is an
// ordinary rename. The cache must follow it and must keep serving.
func TestThirdUpstreamDirectoryFileRename(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	cases := []struct{ name, from, to string }{
		{"long-to-short", "topic/x", "topic"},
		{"short-to-long", "topic", "topic/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := gittest.New(t)
			f.Git(t, f.Bare, "update-ref", "refs/heads/"+tc.from, f.Commit)
			cfg := reviewConfig(t, f)
			c := thirdInit(t, cfg)
			if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/"+tc.from); got == "" {
				t.Fatal("fixture: branch not mirrored")
			}
			f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/"+tc.from)
			f.Git(t, f.Bare, "update-ref", "refs/heads/"+tc.to, f.Commit)
			fetchErr := c.ForceFetch(t.Context(), gittest.Repository)
			_, markerErr := os.Stat(filepath.Join(f.Store(), "oberth-reconcile"))
			serveErr := thirdServes(t.Context(), c, f)
			secondFetch := c.ForceFetch(t.Context(), gittest.Repository)
			_ = c.Close()
			reopened, openErr := gitcache.New(t.Context(), cfg)
			var reopenServe, reopenFetch error
			if openErr == nil {
				reopenServe = thirdServes(t.Context(), reopened, f)
				reopenFetch = reopened.ForceFetch(t.Context(), gittest.Repository)
				_ = reopened.Close()
			}
			t.Logf("fetch after rename: %v", fetchErr)
			t.Logf("marker left: %v", markerErr == nil)
			t.Logf("read after failed fetch: %v", serveErr)
			t.Logf("second fetch: %v", secondFetch)
			t.Logf("reopen: %v; read: %v; fetch: %v", openErr, reopenServe, reopenFetch)
			t.Logf("public refs now: %q", f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/"))
			if fetchErr != nil || serveErr != nil || secondFetch != nil ||
				openErr != nil || reopenServe != nil || reopenFetch != nil {
				t.Fatal("an ordinary upstream branch rename broke the repository")
			}
			ref := "refs/heads/" + tc.to
			if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", ref); got != ref {
				t.Fatal("renamed branch not mirrored", got)
			}
		})
	}
}

// A locally accepted branch fix/1 exists only in the cache; upstream then
// creates branch fix. Refresh must not wedge and reads must keep working.
func TestThirdLocalBranchVersusUpstreamParentName(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	var conflicts []string
	cfg.Log = func(reason, detail string) { conflicts = append(conflicts, reason+": "+detail) }
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "update-ref", "refs/heads/fix/1", f.Commit)
	f.Git(t, f.Bare, "update-ref", "refs/heads/fix", f.Commit)
	fetchErr := c.ForceFetch(t.Context(), gittest.Repository)
	serveErr := thirdServes(t.Context(), c, f)
	_, markerErr := os.Stat(filepath.Join(f.Store(), "oberth-reconcile"))
	t.Logf("fetch: %v", fetchErr)
	t.Logf("read after: %v; marker left: %v", serveErr, markerErr == nil)
	if serveErr != nil {
		t.Fatal("repository stopped serving after a refresh that could not apply")
	}
	if fetchErr != nil {
		t.Error("refresh wedged by a local branch below an upstream branch name")
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/fix/1"); got != f.Commit {
		t.Fatal("local branch changed", got)
	}
	if len(conflicts) == 0 || !strings.Contains(strings.Join(conflicts, "\n"), "refs/heads/fix") {
		t.Fatal("conflicting public creation was silent", conflicts)
	}
}

// Native preparation waits for an existing repository operation to release its gate.
func TestThirdAdmittedPushWaitsForRepositoryGate(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	var c *gitcache.Cache
	release := make(chan func(), 1)
	cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
		unlock, err := gitcache.HoldReceiveRepository(ctx, c, gittest.Repository)
		if err == nil {
			release <- unlock
		}
		return err
	}
	c = thirdInit(t, cfg)
	waiting := make(chan struct{})
	gitcache.BeforeReceivePreparation(c, func() { close(waiting) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.Receive(ctx, gitcache.ReceiveRequest{
			Repository: gittest.Repository, Actor: "a", Fingerprint: "f", AlwaysRun: true,
			Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
			Input:     receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", NewSHA: f.Commit}),
			Output:    io.Discard,
			Deliver:   func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }})
	}()
	var unlock func()
	select {
	case unlock = <-release:
		<-waiting
	case err := <-done:
		t.Fatal("push did not reach admission", err)
	}
	before := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/topic")
	unlock()
	err := <-done
	got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/topic")
	if err != nil || before != "" || got == "" {
		t.Fatal("native preparation did not wait for and finish after the repository gate", before, got, err)
	}
}

// The receive checks live in hooks. If the hook program cannot run, the push
// must be refused, not accepted without any check.
func TestThirdMissingHookProgramFailsClosed(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"missing", "not-executable"} {
		t.Run(kind, func(t *testing.T) {
			f := gittest.New(t)
			cfg := reviewConfig(t, f)
			hook := filepath.Join(f.Root, "hook-"+kind)
			body, err := os.ReadFile(cfg.HookBinary)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(hook, body, 0700); err != nil {
				t.Fatal(err)
			}
			cfg.HookBinary = hook
			called := false
			cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
				called = true
				return nil
			}
			c := thirdInit(t, cfg)
			if kind == "not-executable" {
				err = os.Chmod(hook, 0600)
			} else {
				err = os.Remove(hook)
			}
			if err != nil {
				t.Fatal(err)
			}

			f.Write(t, "file.txt", []byte("unreviewed\n"))
			next := f.CommitFile(t, "unreviewed")
			delivered := 0
			err = c.Receive(t.Context(), gitcache.ReceiveRequest{
				Repository: gittest.Repository, Actor: "agent", Fingerprint: "key", AlwaysRun: true,
				Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
				Input:     receiveInput(t, f, f.Commit, next), Output: io.Discard,
				Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
					delivered++
					return nil, nil
				}})
			head := f.Git(t, f.Store(), "rev-parse", "refs/heads/main")
			t.Logf("receive: %v; admission called: %v; delivered: %d; default branch moved: %v",
				err, called, delivered, head == next)
			if head == next {
				t.Fatal("agent push moved the default branch with no hook, no admission and no delivery")
			}
		})
	}
}
