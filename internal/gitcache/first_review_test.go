package gitcache_test

import (
	"context"
	"k8s.io/utils/clock"
	"strconv"
	"strings"
	"sync"

	"io"
	"path/filepath"

	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestReviewStalledPushBlocksOtherRepository(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	other := "origin/team/other"
	if err := c.Init(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	f.Git(
		t,
		filepath.Join(f.Root, "cache", "origin", "team", "other.git"),
		"fetch",
		f.Bare,
		"refs/heads/main:refs/heads/main",
	)
	stallR, stallW := io.Pipe()
	defer func() { _ = stallW.Close() }()
	begun := make(chan struct{})
	ctxA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	doneA := make(chan error, 1)
	go func() {
		r := gateReceive(f)
		r.Input = stallR
		r.Begin = func() { close(begun) }
		doneA <- c.Receive(ctxA, r)
	}()
	<-begun
	r := gateReceive(f)
	r.Repository = other
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: f.Commit})
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	ctxB, cancelB := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelB()
	start := (clock.RealClock{}).Now()
	err := c.Receive(ctxB, r)
	t.Logf(
		"push to unrelated repository while another push stalls: err=%v after %v",
		err,
		(clock.RealClock{}).Now().Sub(start).Round(time.Millisecond),
	)
	if err != nil {
		t.Fatal("unrelated push blocked", err)
	}
	cancelA()
	<-doneA
}

func TestReviewPromotionResultSurvivesGC(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil })
	f.Git(t, f.Work, "checkout", "-b", "feature")
	f.Write(t, "feature", []byte("feature"))
	next := f.CommitFile(t, "feature")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	target := gitport.Ref{Name: "refs/heads/main", ObjectSHA: f.Commit, CommitSHA: f.Commit}
	request := gitport.PromotionRequest{
		Repository:    gittest.Repository,
		Source:        sourcecontract.Revision{Repository: gittest.Repository, Commit: next},
		Target:        target,
		ActionReceipt: "promotion",
	}
	result, err := c.PreparePromotion(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("keep refs before GC:", f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"))
	if err = c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Log("keep refs after GC:", f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"))
	_, err = c.Publish(t.Context(), promotionPublication(f, result))
	if err != nil {
		t.Fatal("prepared promotion lost to GC", err)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs != "" {
		t.Fatal("published promotion kept its roots", refs)
	}
}

func TestReviewBackslashFileName(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Write(t, `docs\windows.txt`, []byte("x"))
	commit := f.CommitFile(t, "backslash")
	f.Git(t, f.Work, "push", f.Bare, "main")
	c := f.Cache(t, nil)
	b := f.Binding()
	b.Revision.Commit = commit
	b.Export = sourcecontract.Export{
		Ref: sourcecontract.Ref{Name: "refs/heads/main", ObjectSHA: commit, CommitSHA: commit},
	}
	stage, err := c.Stage(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	files := readArchive(t, stage.Body)
	if string(files[`docs\windows.txt`]) != "x" {
		t.Fatal("backslash file missing")
	}
}

func TestAbandonPromotionReleasesOnlyItsPreparation(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil })
	target := gitport.Ref{Name: "refs/heads/main", ObjectSHA: f.Commit, CommitSHA: f.Commit}
	req := gitport.PromotionRequest{
		Repository:    gittest.Repository,
		Source:        sourcecontract.Revision{Repository: gittest.Repository, Commit: f.Commit},
		Target:        target,
		ActionReceipt: "abandon",
	}
	if _, err := c.PreparePromotion(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := c.AbandonPromotion(t.Context(), gittest.Repository, "other"); err != nil {
		t.Fatal(err)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs == "" {
		t.Fatal("different action released preparation")
	}
	if err := c.AbandonPromotion(t.Context(), gittest.Repository, req.ActionReceipt); err != nil {
		t.Fatal(err)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs != "" {
		t.Fatal("abandoned preparation retained", refs)
	}
}

func TestReviewHungUpstreamDoesNotBlockOtherRepositories(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	for _, repo := range []string{gittest.Repository, "origin/team/slow"} {
		if err := c.Init(t.Context(), repo); err != nil {
			t.Fatal(err)
		}
	}
	reviewFault(t, f, map[string]any{"args": []string{"ls-remote"}, "stop": true})
	done := make(chan error, 1)
	go func() { _, err := c.RemoteRef(t.Context(), "origin/team/slow", "refs/heads/main"); done <- err }()
	marker := reviewWait(t, filepath.Join(f.Root, "stopped"))
	pid, err := strconv.Atoi(strings.Split(string(marker), "\n")[0])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err = c.Keep(ctx, "run", f.Bindings(f.Commit)); err != nil {
		t.Fatal("unrelated Keep blocked", err)
	}
	if _, err = c.ReadFile(ctx, f.Binding().Revision, "file.txt"); err != nil {
		t.Fatal("unrelated ReadFile blocked", err)
	}
	stage, err := c.Stage(ctx, f.Binding())
	if err != nil {
		t.Fatal("unrelated Stage blocked", err)
	}
	_ = stage.Body.Close()
	reviewDisarm(t, f)
	if err = resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
func TestKeepDuringOwnerSnapshotSurvivesCollection(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cfg.LiveOwners = func(ctx context.Context, _ string) (gitcache.OwnerPage, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return gitcache.OwnerPage{}, nil
		case <-ctx.Done():
			return gitcache.OwnerPage{}, context.Cause(ctx)
		}
	}
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.GC(t.Context()) }()
	<-entered
	if err := c.Keep(t.Context(), "new-run", f.Bindings(f.Commit)); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs == "" {
		t.Fatal("new owner lost during collection")
	}
	if err := c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs != "" {
		t.Fatal("finished owner retained forever")
	}
}

func TestArchiveAndRemoveRefreshFinishedOwners(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	for _, operation := range []string{"archive", "remove"} {
		t.Run(operation, func(t *testing.T) {
			f := gittest.New(t)
			c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil })
			if err := c.Keep(t.Context(), "finished", f.Bindings(f.Commit)); err != nil {
				t.Fatal(err)
			}
			if operation == "remove" {
				if err := c.Remove(t.Context(), gittest.Repository); err != nil {
					t.Fatal(err)
				}
			} else {
				body, err := c.Archive(t.Context(), gittest.Repository)
				if err != nil {
					t.Fatal(err)
				}
				if err = body.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
