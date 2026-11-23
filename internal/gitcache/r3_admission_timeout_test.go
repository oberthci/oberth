package gitcache_test

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

// The supplied non-returning callback probe must finish even with no caller
// deadline, and a second push must work while that callback is still blocked.
func TestAdmissionTimeoutReleasesReceiveWithoutCallerDeadline(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	release, exited := make(chan struct{}), make(chan struct{})
	source := make(chan *gitcache.ReceiveSource, 1)
	var once sync.Once
	var calls, metrics, logs, aborts atomic.Int32
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		if calls.Load() > 0 {
			<-exited
		}
	})
	cfg.PreReceive = func(_ context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		if calls.Add(1) > 1 {
			return nil
		}
		source <- s
		<-release
		close(exited)
		return nil
	}
	cfg.AdmissionMetric = func(string) { metrics.Add(1) }
	cfg.Log = func(string, string) { logs.Add(1) }
	cfg.AbortReceive = func(string) { aborts.Add(1) }
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	restore := gitcache.SetAdmissionTimeout(c, 150*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := gateReceive(f)
	request.AlwaysRun = true
	request.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: f.Commit})
	request.Output = io.Discard
	request.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	var diagnostics bytes.Buffer
	request.Diagnostics = &diagnostics
	done := make(chan error, 1)
	go func() { done <- c.Receive(ctx, request) }()
	var scoped *gitcache.ReceiveSource
	select {
	case scoped = <-source:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("admission was not reached")
	}
	expectAdmissionCutoff(t, done, &diagnostics, cancel)
	// Only the abandoned admission needs the short deadline. The follow-up
	// checks that the same cache released the receive slot under its normal budget.
	restore()
	checkTimedOutAdmission(t, c, scoped, f)
	observed := []int32{metrics.Load(), logs.Load(), aborts.Load()}
	if ctx.Err() != nil || !slices.Equal(observed, []int32{1, 1, 1}) {
		t.Fatal("deadline observations", ctx.Err(), observed)
	}
}
func checkTimedOutAdmission(t *testing.T, c *gitcache.Cache, scoped *gitcache.ReceiveSource, f *gittest.Fixture) {
	t.Helper()
	_, err := scoped.ReadFile(context.Background(),
		gitport.Revision{Repository: gittest.Repository, Commit: f.Commit}, "file.txt")
	if err == nil {
		t.Fatal("late callback source remained usable")
	}
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/feature"); got != "" {
		t.Fatal("timed out push moved refs")
	}
	request := gateReceive(f)
	request.AlwaysRun = true
	request.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/next", NewSHA: f.Commit})
	request.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err := c.Receive(t.Context(), request); err != nil {
		t.Fatal("next push was blocked by abandoned callback", err)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/next"); got != f.Commit {
		t.Fatal("next push did not move its ref", got)
	}
}

func expectAdmissionCutoff(t *testing.T, done <-chan error, diagnostics *bytes.Buffer, cancel context.CancelFunc) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(diagnostics.String(), "push admission did not finish in time. Nothing ran and no ref moved; push the same commit again.") {
			t.Fatal("deadline feedback", err, diagnostics.String())
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("independent admission deadline did not refuse the push")
	}
}
