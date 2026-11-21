package gitcache_test

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestDefaultFileDisappearingCommitNeverMeansMissingPolicy(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "file.txt", []byte("new policy"))
	next := f.CommitFile(t, "policy")
	f.Git(t, f.Work, "push", f.Bare, "main")
	reviewFault(t, f, map[string]any{"args": []string{"cat-file", "--batch-check=%(objecttype)"}, "stop": true})
	type result struct {
		head gitport.Ref
		err  error
	}
	done := make(chan result, 1)
	go func() {
		head, _, err := c.DefaultFile(t.Context(), gittest.Repository, "file.txt")
		done <- result{head, err}
	}()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(reviewWait(t, filepath.Join(f.Root, "stopped")))))
	f.Git(t, f.Store(), "gc", "--prune=now")
	reviewDisarm(t, f)
	if err := resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	got := <-done
	var unavailable *gitport.UnavailableError
	var absent *gitport.AbsentError
	if got.head.CommitSHA != next || !errors.As(got.err, &unavailable) || errors.As(got.err, &absent) {
		t.Fatal("disappearing commit looked like absent policy", got.head, got.err)
	}
}
