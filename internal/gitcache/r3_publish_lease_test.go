package gitcache_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPublishLeaseProtectsRefChangedAfterPreflight(t *testing.T) {
	t.Parallel()
	p := newPublishFixture(t)
	reviewFault(t, p.f, map[string]any{"args": []string{"push"}, "stop": true})
	done := make(chan error, 1)
	go func() { _, err := p.cache.Publish(t.Context(), p.intent); done <- err }()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(reviewWait(t, filepath.Join(p.f.Root, "stopped")))))
	p.upstream(t, p.f.Commit)
	reviewDisarm(t, p.f)
	if err := resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	if err := <-done; publicationClass(err) != "moved(upstream)" {
		t.Fatal("lost lease was not an upstream movement", err)
	}
	if got := p.f.Git(t, p.f.Bare, "rev-parse", p.intent.Ref); got != p.f.Commit {
		t.Fatal("lease overwrote the concurrent upstream ref", got)
	}
}
