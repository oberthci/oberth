package gitcache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
)

type waitingFeedback chan string

func (w waitingFeedback) Write(p []byte) (int, error) {
	select {
	case w <- string(p):
	default:
	}
	return len(p), nil
}

func TestSecondPushReportsWaitingThenContinues(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	reviewFault(t, f, map[string]any{"args": []string{"receive-pack"}, "stop": true})
	r := gateReceive(f)
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/first", NewSHA: f.Commit})
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	first := make(chan error, 1)
	go func() { first <- c.Receive(t.Context(), r) }()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(reviewWait(t, filepath.Join(f.Root, "stopped")))))
	feedback := make(waitingFeedback, 8)
	second := make(chan error, 1)
	r2 := gateReceive(f)
	r2.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/second", NewSHA: f.Commit})
	r2.Diagnostics = feedback
	r2.Deliver = r.Deliver
	go func() { second <- c.Receive(t.Context(), r2) }()
	select {
	case line := <-feedback:
		if !strings.Contains(line, "waiting for another push") {
			t.Fatal(line)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting push was silent")
	}
	reviewDisarm(t, f)
	if err := resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	for _, done := range []chan error{first, second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("push did not continue")
		}
	}
}

func TestPublishDifferentRefsDoesNotShareNetworkPermit(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"refs/heads/first", "refs/heads/second"} {
		f.Git(t, f.Store(), "update-ref", ref, f.Commit)
	}
	reviewFault(t, f, map[string]any{"args": []string{"push"}, "stop": true})
	done := make(chan error, 1)
	go func() {
		_, err := c.Publish(t.Context(), publicationFor(f, "refs/heads/first", f.Commit, ""))
		done <- err
	}()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(reviewWait(t, filepath.Join(f.Root, "stopped")))))
	reviewDisarm(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := c.Publish(ctx, publicationFor(f, "refs/heads/second", f.Commit, "")); err != nil {
		t.Fatal("unrelated ref held network permit", err)
	}
	if err := resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPromotionResultPublishesAfterRestartAndRetries(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "new", []byte("new"))
	next := f.CommitFile(t, "promotion")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	p,
		err := c.PreparePromotion(t.Context(),
		gitport.PromotionRequest{Repository: gittest.Repository,
			Source: gitport.Revision{Repository: gittest.Repository,
				Commit: next},
			Target: gitport.Ref{Name: "refs/heads/main",
				ObjectSHA: f.Commit,
				CommitSHA: f.Commit},
			ActionReceipt: "prepared"})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = reviewNew(t, cfg)
	r := publicationFor(f, p.Result.Name, p.Result.ObjectSHA, f.Commit)
	r.PromotionID = p.Request.ActionReceipt
	for range 2 {
		if _, err = c.Publish(t.Context(), r); err != nil {
			t.Fatal("prepared result/retry lost", err)
		}
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs != "" {
		t.Fatal("settled promotion stayed pinned", refs)
	}
}

func TestUpstreamPermanentRefusalIsNotAnOutage(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	ref := "refs/heads/topic"
	f.Git(t, f.Bare, "update-ref", ref, f.Commit)
	f.Git(t, f.Bare, "config", "receive.denyNonFastForwards", "true")
	f.Git(t, f.Work, "checkout", "--orphan", "rewrite")
	f.Write(t, "rewritten", []byte("rewritten"))
	next := f.CommitFile(t, "rewrite")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	f.Git(t, f.Store(), "update-ref", ref, next)
	_, err := c.Publish(t.Context(), publicationFor(f, ref, next, f.Commit))
	var unavailable *gitport.UnavailableError
	if !publicerr.Permanent(err) || errors.As(err, &unavailable) || !strings.Contains(err.Error(), "non-fast-forward") {
		t.Fatal("permanent upstream refusal was lost", err)
	}
	if strings.Contains(err.Error(), "could not finish") || strings.Contains(err.Error(), "server is available") {
		t.Fatal("permanent rejection included outage guidance", err)
	}
}

func TestReceiveDefaultFileUsesFreshHeadAndExpires(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	var source *gitcache.ReceiveSource
	cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		source = s
		head, file, err := s.DefaultFile(ctx, gittest.Repository, "file.txt")
		if err != nil || head.CommitSHA != f.Commit || string(file.Bytes) != "exact source\n" {
			t.Error(head, file, err)
		}
		return err
	}
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/test", NewSHA: f.Commit})
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err := c.Receive(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.DefaultFile(t.Context(), "origin/team/other", "file.txt"); !errors.Is(err, context.Canceled) {
		t.Fatal("reader escaped its admission lifetime", err)
	}
}

func TestInterruptedMirrorRepairsOnNextReadWithoutOverwritingAcceptedHead(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Git(t, f.Bare, "update-ref", "refs/heads/local", f.Commit)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "local", []byte("accepted"))
	local := f.CommitFile(t, "accepted")
	f.Git(t, f.Store(), "fetch", f.Work, local)
	f.Git(t, f.Store(), "update-ref", "refs/heads/local", local)
	f.Git(t, f.Work, "checkout", "--detach", f.Commit)
	f.Write(t, "upstream", []byte("upstream"))
	next := f.CommitFile(t, "upstream")
	f.Git(t, f.Work, "push", f.Bare, next+":refs/heads/main")
	f.Git(t, f.Bare, "update-ref", "refs/heads/local", next)
	reviewFault(t,
		f,
		map[string]any{"args": []string{"update-ref",
			"--no-deref"},
			"needle": "refs/oberth/upstream/heads/main",
			"phase":  "after"})
	done := make(chan error, 1)
	go func() { done <- c.ForceFetch(t.Context(), gittest.Repository) }()
	reviewKill(t, f)
	<-done
	reviewDisarm(t, f)
	if _, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	public := f.Git(t, f.Store(), "rev-parse", "refs/heads/main")
	mirror := f.Git(t, f.Store(), "rev-parse", "refs/oberth/upstream/heads/main")
	if public != mirror {
		t.Fatal("next operation did not reconcile", public, mirror)
	}
	if actual := f.Git(t, f.Store(), "rev-parse", "refs/heads/local"); actual != local {
		t.Fatal("accepted local ref overwritten", actual, local)
	}
	if _, err := os.Stat(filepath.Join(f.Store(), "oberth-reconcile")); !os.IsNotExist(err) {
		t.Fatal("completed reconciliation marker remains", err)
	}
}
