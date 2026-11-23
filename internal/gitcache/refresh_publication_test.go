package gitcache_test

import (
	"context"
	"errors"
	"io"
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

func TestDemandRefreshCannotOverwriteConcurrentPublication(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "promoted", []byte("promoted"))
	next := f.CommitFile(t, "promoted")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	p, err := c.PreparePromotion(t.Context(), gitport.PromotionRequest{Repository: gittest.Repository,
		ActionReceipt: "promotion", Source: gitport.Revision{Repository: gittest.Repository, Commit: next},
		Target: gitport.Ref{Name: "refs/heads/main", ObjectSHA: f.Commit, CommitSHA: f.Commit}})
	if err != nil {
		t.Fatal(err)
	}
	// Stop after the private fetch has captured the old forge head, before import.
	reviewFault(t, f, map[string]any{"args": []string{"for-each-ref", "refs/"}, "stop": true})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	fetched, published := make(chan error, 1), make(chan error, 1)
	go func() { fetched <- c.ForceFetch(ctx, gittest.Repository) }()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(reviewWait(t, filepath.Join(f.Root, "stopped")))))
	defer func() { _ = resumeStopped(t, pid) }()
	reviewDisarm(t, f)
	go func() { _, err := c.Publish(ctx, promotionPublication(f, p)); published <- err }()
	waitForForgeHead(t, f, p.Result.CommitSHA)
	if err := resumeStopped(t, pid); err != nil {
		t.Fatal(err)
	}
	for _, done := range []chan error{fetched, published} {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	assertAdvertisement(t, c, p.Result.CommitSHA)
	if got := f.Git(t, f.Store(), "rev-parse", "refs/oberth/upstream/heads/main"); got != p.Result.CommitSHA {
		t.Fatal("older snapshot replaced confirmed publication", got)
	}
}

func waitForForgeHead(t *testing.T, f *gittest.Fixture, want string) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for {
		if f.Git(t, f.Bare, "rev-parse", "refs/heads/main") == want {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("publication network work waited behind the fetch")
		}
	}
}

// requireBehindReceive requires the retryable refusal of a publication whose
// cache record waits for a pending receive, marked as such.
func requireBehindReceive(t *testing.T, err error) {
	t.Helper()
	if err == nil || publicerr.Classify(err).Code() != publicerr.Unavailable {
		t.Fatal("pending cache update must remain retryable", err)
	}
	var pending *publicerr.Error
	if !errors.As(err, &pending) || pending.ID != gitport.ReceivePendingID {
		t.Fatal("a publication behind a pending receive is not marked as such", err)
	}
}

func TestPublicationCacheWaitsForPendingReceive(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "promoted", []byte("promoted"))
	next := f.CommitFile(t, "promoted")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	p, err := c.PreparePromotion(t.Context(), gitport.PromotionRequest{Repository: gittest.Repository,
		ActionReceipt: "promotion", Source: gitport.Revision{Repository: gittest.Repository, Commit: next},
		Target: gitport.Ref{Name: "refs/heads/main", ObjectSHA: f.Commit, CommitSHA: f.Commit}})
	if err != nil {
		t.Fatal(err)
	}
	r := gateReceive(f)
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/pending", NewSHA: f.Commit})
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, io.ErrClosedPipe }
	if err := c.Receive(t.Context(), r); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	before := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/")
	intent := promotionPublication(f, p)
	intent.Attempts = 1
	_, err = c.Publish(t.Context(), intent)
	requireBehindReceive(t, err)
	if after := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/"); after != before {
		t.Fatal("publication changed the pending receive inventory", before, after)
	}
	if got := f.Git(t, f.Bare, "rev-parse", "refs/heads/main"); got != p.Result.CommitSHA {
		t.Fatal("fixture did not publish remotely", got)
	}
	if err = c.ReplayPending(t.Context(), func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	intent.Attempts = 2
	if _, err = c.Publish(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	assertAdvertisement(t, c, p.Result.CommitSHA)
	commands, err := os.ReadFile(filepath.Join(f.Root, "commands.jsonl"))
	if err != nil || strings.Count(string(commands), `"push"`) != 1 {
		t.Fatal("cache retry repeated the native push", err)
	}
}
