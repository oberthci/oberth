package gitcache_test

import (
	"context"
	"fmt"
	"io"
	"k8s.io/utils/clock"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestDeltaMirrorKilledBetweenRefRenames(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	for _, stop := range []struct {
		needle,
		phase string
	}{{"refs/oberth/upstream/heads/main",
		"before"},
		{"refs/oberth/upstream/heads/main",
			"after"},
		{"refs/heads/main",
			"before"},
		{"refs/heads/main",
			"after"}} {
		t.Run(stop.needle+"-"+stop.phase, func(t *testing.T) {
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
			reviewFault(t,
				f,
				map[string]any{"args": []string{"update-ref",
					"--no-deref"},
					"needle": stop.needle,
					"phase":  stop.phase})
			done := make(chan error, 1)
			go func() { done <- c.ForceFetch(t.Context(), gittest.Repository) }()
			reviewKill(t, f)
			t.Log("interrupted fetch:", <-done)
			reviewDisarm(t, f)
			_ = c.Close()
			c = reviewNew(t, cfg)
			public := f.Git(t, f.Store(), "rev-parse", "refs/heads/main")
			tracking := f.Git(t, f.Store(), "rev-parse", "refs/oberth/upstream/heads/main")
			t.Logf("after reopen: initial=%s next=%s public=%s tracking=%s", f.Commit, next, public, tracking)
			if public != tracking {
				t.Error("interrupted transaction left mixed public and tracking generations")
			}
			f.Write(t, "third", []byte("third"))
			third := f.CommitFile(t, "third")
			f.Git(t, f.Work, "push", f.Bare, "main")
			if e := c.ForceFetch(t.Context(), gittest.Repository); e != nil {
				t.Fatal(e)
			}
			public = f.Git(t, f.Store(), "rev-parse", "refs/heads/main")
			tracking = f.Git(t, f.Store(), "rev-parse", "refs/oberth/upstream/heads/main")
			t.Logf("after retry: newest=%s public=%s tracking=%s", third, public, tracking)
			if public != third {
				t.Error("retry permanently preserves interrupted mirror as an accepted local head")
			}
			f.Git(t, f.Store(), "fsck", "--full")
		})
	}
}

func TestDeltaKilledKeepPreservesLiveQuarantine(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	entered := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(resume) })
	cfg.PreReceive = func(ctx context.Context, e gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		r := gitport.Revision{Repository: gittest.Repository, Commit: e.Updates[0].NewSHA}
		if _, err := s.ReadFile(ctx, r, "next"); err != nil {
			return err
		}
		close(entered)
		select {
		case <-resume:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := s.ReadFile(ctx, r, "next")
		return err
	}
	c := reviewNew(t, cfg)
	if e := c.Init(t.Context(), gittest.Repository); e != nil {
		t.Fatal(e)
	}
	f.Write(t, "next", []byte("next"))
	next := f.CommitFile(t, "next")
	receive := make(chan error, 1)
	go func() {
		receive <- c.Receive(t.Context(),
			gitcache.ReceiveRequest{Repository: gittest.Repository,
				Actor:       "a",
				Fingerprint: "f",
				Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main",
					SHA: f.Commit},
				AlwaysRun: true,
				Input: receiveRefInput(t,
					f,
					gitcache.RefUpdate{Ref: "refs/heads/topic",
						NewSHA: next}),
				Output: io.Discard,
				Deliver: func(context.Context,
					gitcache.ReceiveEvent) ([]string,
					error) {
					return nil,
						nil
				}})
	}()
	select {
	case <-entered:
	case e := <-receive:
		t.Fatal("receive ended before callback", e)
	case <-time.After(10 * time.Second):
		t.Fatal("no gate callback")
	}
	before, _ := filepath.Glob(filepath.Join(f.Store(), "objects", "tmp_objdir-incoming-*"))
	reviewFault(t,
		f,
		map[string]any{"args": []string{"update-ref"},
			"needle": "refs/oberth/keep/victim/",
			"phase":  "before"})
	keep := make(chan error, 1)
	go func() { keep <- c.Keep(t.Context(), "victim", f.Bindings(f.Commit)) }()
	reviewKill(t, f)
	t.Log("killed Keep:", <-keep)
	reviewDisarm(t, f)
	after, _ := filepath.Glob(filepath.Join(f.Store(), "objects", "tmp_objdir-incoming-*"))
	t.Logf("live quarantine before=%v after=%v", before, after)
	once.Do(func() { close(resume) })
	if e := <-receive; e != nil {
		t.Errorf("unrelated killed Keep broke active receive: %v", e)
	}
	if len(before) != 1 || len(after) != 1 {
		t.Error("live receive quarantine removed by another command's cleanup")
	}
}

func TestDeltaNonReturningAdmissionDeadline(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		close(entered)
		<-release
		return nil
	}
	c := reviewNew(t, cfg)
	if e := c.Init(t.Context(), gittest.Repository); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 750*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.Receive(ctx,
			gitcache.ReceiveRequest{Repository: gittest.Repository,
				Actor:       "a",
				Fingerprint: "f",
				AlwaysRun:   true,
				Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main",
					SHA: f.Commit},
				Input: receiveRefInput(t,
					f,
					gitcache.RefUpdate{Ref: "refs/heads/topic",
						NewSHA: f.Commit}),
				Output: io.Discard})
	}()
	select {
	case <-entered:
	case e := <-done:
		t.Fatal("callback not reached", e)
	}
	<-ctx.Done()
	start := (clock.RealClock{}).Now()
	returned := false
	select {
	case e := <-done:
		returned = true
		t.Log("deadline result", e)
	case <-time.After(5 * time.Second):
		t.Error("Receive still blocked five seconds after caller deadline while callback ignores cancellation")
	}
	once.Do(func() { close(release) })
	if !returned {
		select {
		case e := <-done:
			t.Logf("returned only after callback released, late by %s: %v", (clock.RealClock{}).Now().Sub(start), e)
		case <-time.After(5 * time.Second):
			t.Fatal("receive did not join")
		}
	}
	if _, e := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/topic"); e == nil {
		t.Error("deadline moved ref")
	}
}

func TestDeltaExportBatchAndCrash(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if e := c.Init(t.Context(), gittest.Repository); e != nil {
		t.Fatal(e)
	}
	blob := f.Git(t, f.Work, "rev-parse", "HEAD:file.txt")
	var tree strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&tree, "100644 blob %s\tf%04d\n", blob, i)
	}
	commit := commitTree(t, f, tree.String())
	f.Git(t, f.Store(), "fetch", f.Work, commit)
	if e := os.WriteFile(filepath.Join(f.Root, "commands.jsonl"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	start := (clock.RealClock{}).Now()
	stage, e := c.Stage(t.Context(), f.Bindings(commit)[0])
	if e != nil {
		t.Fatal(e)
	}
	_ = stage.Body.Close()
	data, _ := os.ReadFile(filepath.Join(f.Root, "commands.jsonl"))
	batches := strings.Count(string(data), `"cat-file", "--batch"`)
	t.Logf("1000-file Stage=%s; blob batch processes=%d; all native processes=%d",
		(clock.RealClock{}).Now().Sub(start),
		batches,
		strings.Count(string(data),
			"\n"))
	if batches != 1 {
		t.Error("blob export not one process")
	}
	_ = c.Close()
	reviewFault(t, f, map[string]any{"args": []string{"cat-file", "--batch"}, "stop": true})
	binary, _ := os.Executable()
	worker := exec.Command(binary, "-test.run=^TestDeltaExportCrashWorker$")
	// Disable the race runtime's exit sleep so the watchdog measures the worker's work.
	worker.Env = append(os.Environ(),
		"GORACE=atexit_sleep_ms=0",
		"DELTA_EXPORT_ROOT="+f.Root,
		"DELTA_EXPORT_COMMIT="+commit,
		"DELTA_EXPORT_INITIAL="+f.Commit)
	if e = worker.Start(); e != nil {
		t.Fatal(e)
	}
	joined := make(chan error, 1)
	go func() { joined <- worker.Wait() }()
	reviewKill(t, f)
	select {
	case e = <-joined:
		if e != nil {
			t.Error("export worker", e)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Error("killed cat-file left Stage stuck beyond 500ms caller deadline; stopping own test worker")
		_ = worker.Process.Kill()
		<-joined
	}
	reviewDisarm(t, f)
	c = reviewNew(t, cfg)
	stage, e = c.Stage(t.Context(), f.Bindings(commit)[0])
	if e != nil {
		t.Fatal("retry export after restart", e)
	}
	_ = stage.Body.Close()
	entries, _ := os.ReadDir(cfg.Scratch)
	if len(entries) != 0 {
		t.Error("export debris", entries)
	}
}

func TestDeltaExportCrashWorker(t *testing.T) {
	root := os.Getenv("DELTA_EXPORT_ROOT")
	if root == "" {
		t.Skip("only an explicitly started helper")
	}
	f := &gittest.Fixture{Root: root,
		Work: filepath.Join(root,
			"author"),
		Bare: filepath.Join(root,
			"upstream.git"),
		Binary: "/usr/bin/git",
		Commit: os.Getenv("DELTA_EXPORT_INITIAL")}
	cfg := reviewConfig(t, f)
	cfg.Binary = filepath.Join(root, "git-wrapper")
	c := reviewNew(t, cfg)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	stage, e := c.Stage(ctx, f.Bindings(os.Getenv("DELTA_EXPORT_COMMIT"))[0])
	if stage.Body != nil {
		_ = stage.Body.Close()
	}
	if e == nil {
		t.Fatal("killed export unexpectedly succeeded")
	}
}

func TestDeltaAdmissionHasBoundedNativeWork(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	calls := 0
	cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, source *gitcache.ReceiveSource) error {
		calls++
		file, err := source.ReadFile(ctx, gitport.Revision{Repository: gittest.Repository, Commit: f.Commit}, "file.txt")
		if err == nil && len(file.Bytes) == 0 {
			return fmt.Errorf("admission received empty source")
		}
		return err
	}
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	commands := filepath.Join(f.Root, "commands.jsonl")
	if err := os.WriteFile(commands, nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := c.Receive(t.Context(), gitcache.ReceiveRequest{
		Repository: gittest.Repository, Actor: "a", Fingerprint: "f", AlwaysRun: true,
		Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
		Input:     receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", NewSHA: f.Commit}), Output: io.Discard,
		Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil },
	})
	if err != nil || calls != 1 {
		t.Fatal("admission did not complete once", calls, err)
	}
	log, err := os.ReadFile(commands)
	if err != nil {
		t.Fatal(err)
	}
	processes := strings.Count(string(log), "\n")
	t.Logf("admission callbacks=%d; native processes=%d", calls, processes)
	if processes > 24 || strings.Count(string(log), `"receive-pack"`) != 1 {
		t.Fatal("admission exceeded native work bound", processes, string(log))
	}
}
