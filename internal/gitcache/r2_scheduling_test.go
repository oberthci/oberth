package gitcache_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestSilentLocalGCOutlivesTransferIdleWindow(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	gitcache.DeltaSetNativeIdle(c, 20*time.Millisecond)
	reviewFault(t, f, map[string]any{"args": []string{"gc"}, "stop": true})
	done := make(chan error, 1)
	go func() { done <- c.GC(t.Context()) }()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(reviewWait(t, filepath.Join(f.Root, "stopped")))))
	defer func() { _ = resumeStopped(t, pid) }()
	select {
	case err := <-done:
		t.Fatal("silent local gc spent transport idle", err)
	case <-time.After(120 * time.Millisecond):
	}
	reviewDisarm(t, f)
	if err := resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGCContinuesAfterOneRepositoryFails(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	wrapper := filepath.Join(f.Root, "gc-failure")
	body := "#!/usr/bin/python3\n" +
		"import os,sys\n" +
		"if 'gc' in sys.argv and os.getcwd().endswith('/a.git'):sys.exit(1)\n" +
		"os.execv('/usr/bin/git',['git']+sys.argv[1:])\n" +
		""
	if err := os.WriteFile(wrapper, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = wrapper
	c := reviewNew(t, cfg)
	for _, repo := range []string{"origin/team/a", "origin/team/b"} {
		if err := c.Init(t.Context(), repo); err != nil {
			t.Fatal(err)
		}
		b := f.Bindings(f.Commit)[0]
		b.Revision.Repository = repo
		if err := c.Keep(t.Context(), "orphan", []sourcecontract.Binding{b}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.GC(t.Context()); err == nil || !strings.Contains(err.Error(), "a.git") {
		t.Fatal("failure not attributed", err)
	}
	if refs := f.Git(t,
		filepath.Join(cfg.Root,
			"origin/team/b.git"),
		"for-each-ref",
		"--format=%(refname)",
		"refs/oberth/keep/"); refs != "" {
		t.Fatal("later repository not collected", refs)
	}
}

func TestHungFetchLeavesSameRepositoryReadsAndKeepFree(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	reviewFault(t, f, map[string]any{"args": []string{"fetch", "--progress"}, "stop": true})
	done := make(chan error, 1)
	go func() { done <- c.ForceFetch(t.Context(), gittest.Repository) }()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(reviewWait(t, filepath.Join(f.Root, "stopped")))))
	defer func() { _ = resumeStopped(t, pid) }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _,
		err := c.ReadFile(ctx,
		gitport.Revision{Repository: gittest.Repository,
			Commit: f.Commit},
		"file.txt"); err != nil {
		t.Fatal(err)
	}
	if err := c.Keep(ctx, "run", f.Bindings(f.Commit)); err != nil {
		t.Fatal("hung fetch holds repository gate", err)
	}
	reviewDisarm(t, f)
	if err := resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTwoPublicationsToOneRepositoryConflictWithoutWaiting(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	ref := gitport.Ref{Name: "refs/heads/topic", ObjectSHA: f.Commit, CommitSHA: f.Commit}
	f.Git(t, f.Store(), "update-ref", ref.Name, f.Commit)
	reviewFault(t, f, map[string]any{"args": []string{"push"}, "stop": true})
	done := make(chan error, 1)
	firstCtx, stopFirst := context.WithCancel(t.Context())
	joined := false
	defer func() {
		stopFirst()
		if !joined {
			<-done
		}
	}()
	go func() { _, err := c.Publish(firstCtx, publicationFor(f, ref.Name, f.Commit, "")); done <- err }()

	pid, _ := strconv.Atoi(strings.TrimSpace(string(reviewWait(t, filepath.Join(f.Root, "stopped")))))
	defer func() { _ = resumeStopped(t, pid) }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := c.Publish(ctx, publicationFor(f, ref.Name, f.Commit, ""))
	if err == nil || !strings.Contains(err.Error(), "push is in progress") {
		t.Fatal("concurrent publication did not explain busy state", err)
	}
	reviewDisarm(t, f)
	if err := resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	firstErr := <-done
	joined = true
	if firstErr != nil {
		t.Fatal(firstErr)
	}
}

func TestGateDeadlineReleasesReceiveAndRevokesLateSource(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	entered := make(chan *gitcache.ReceiveSource, 1)
	release := make(chan struct{})
	returned := make(chan struct{})
	var releaseOnce sync.Once
	stop := func() { releaseOnce.Do(func() { close(release) }) }
	defer stop()
	calls, metrics, logs := 0, 0, 0
	cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		calls++
		if calls == 1 {
			entered <- s
			<-release
			close(returned)
		}
		return nil
	}
	cfg.AdmissionMetric = func(reason string) {
		checkAdmissionObserverReason(t, reason, "callback")
		metrics++
	}
	cfg.Log = func(reason, id string) {
		checkAdmissionObserverReason(t, reason, id)
		logs++
	}
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	var diagnostics strings.Builder
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/late", NewSHA: f.Commit})
	r.Diagnostics = &diagnostics
	go func() { done <- c.Receive(ctx, r) }()
	source := <-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline waited for callback")
	}
	checkAdmissionObservations(t, metrics, logs, diagnostics.String())
	if _,
		err := source.ReadFile(t.Context(),
		gitport.Revision{Repository: gittest.Repository,
			Commit: f.Commit},
		"file.txt"); err == nil {
		t.Fatal("late source stayed live")
	}
	r = gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/next", NewSHA: f.Commit})
	r.Output = io.Discard
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err := c.Receive(t.Context(), r); err != nil {
		t.Fatal("receive slot retained by late callback", err)
	}
	if _, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/late"); err == nil {
		t.Fatal("late approval moved ref")
	}
	stop()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("late callback did not finish after release")
	}
}

func checkAdmissionObserverReason(t *testing.T, reason, id string) {
	t.Helper()
	if reason != "admission still running at its deadline" || id == "" {
		t.Error(reason, id)
	}
}
func checkAdmissionObservations(t *testing.T, metrics, logs int, diagnostics string) {
	t.Helper()
	if metrics != 1 || logs != 1 || strings.Count(diagnostics, "Try the same push again") != 1 {
		t.Fatal(metrics, logs, diagnostics)
	}
}
