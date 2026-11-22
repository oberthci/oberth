package gitcache_test

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) add(reason, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, reason+": "+detail)
}

func (l *logLines) has(prefix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// An admission still running at its deadline is logged with the pushed
// repository, so the next incident names it.
func TestStuckAdmissionLogNamesTheRepository(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.AdmissionTimeout = 150 * time.Millisecond
	release, exited := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release); <-exited })
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		defer close(exited)
		<-release
		return nil
	}
	logs := &logLines{}
	cfg.Log = logs.add
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: f.Commit})
	r.Output, r.Diagnostics = io.Discard, io.Discard
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err := c.Receive(t.Context(), r); err == nil {
		t.Fatal("a stuck admission was admitted")
	}
	if !logs.has("admission still running at its deadline: " + gittest.Repository + ": ") {
		t.Fatal("the stuck admission log does not name the repository", logs.lines)
	}
}

// A forge that cannot be reached is logged with the repository whose forge
// it is, both for the failed command and for the cache served instead.
func TestForgeFailureLogNamesTheRepository(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	now := time.Unix(2000000000, 0)
	var mu sync.Mutex
	cfg.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	logs := &logLines{}
	cfg.Log = logs.add
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.Bare, f.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(gitcache.RefreshInterval)
	mu.Unlock()
	var notice strings.Builder
	if err := c.RefreshRead(t.Context(), gittest.Repository, &notice); err != nil {
		t.Fatal("the cache was not served during the outage", err)
	}
	prefix := gitport.ForgeStatusReason + ": " + gittest.Repository + ": " + gitport.ForgeOperatorRemedy
	if !logs.has(prefix+" ") || !logs.has(prefix+" "+gitport.ForgeCacheNotice) {
		t.Fatal("the forge failure log does not name the repository", logs.lines)
	}
}
