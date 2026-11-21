package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

type slowReconcile struct {
	started chan []string
	release chan struct{}
	ctxs    chan context.Context
}

func (s *slowReconcile) run(ctx context.Context, _ string, refs []string) error {
	s.started <- refs
	s.ctxs <- ctx
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func moved(refs ...string) []gitcache.RefUpdate {
	updates := make([]gitcache.RefUpdate, 0, len(refs))
	for _, ref := range refs {
		updates = append(updates, gitcache.RefUpdate{Ref: ref})
	}
	return updates
}

// A reader hands its notice over and returns at once, also while the
// repository's reconciliation is slow; notices taken meanwhile wait as one,
// with every moved ref; the reconciliation is bounded and runs under the
// server's lifetime, not the reader's.
func TestRefNoticeDoesNotHoldItsReader(t *testing.T) {
	slow := &slowReconcile{started: make(chan []string, 4), release: make(chan struct{}), ctxs: make(chan context.Context, 4)}
	notices := newRefNotices(t.Context(), slow.run, func(error) {})
	defer func() { _ = notices.close(t.Context()) }()
	reader, leave := context.WithCancel(t.Context())
	leave()
	notices.changed(reader, "origin/team/widget", moved("refs/heads/nightly"))
	if refs := <-slow.started; !slices.Equal(refs, []string{"refs/heads/nightly"}) {
		t.Fatal("wrong refs", refs)
	}
	if ctx := <-slow.ctxs; ctx.Err() != nil {
		t.Fatal("the reconciliation took the reader's cancellation")
	} else if _, bounded := ctx.Deadline(); !bounded {
		t.Fatal("the reconciliation is not bounded")
	}
	// The worker is busy: both readers return although nothing reconciles them yet.
	notices.changed(t.Context(), "origin/team/widget", moved("refs/heads/weekly"))
	notices.changed(t.Context(), "origin/team/widget", moved("refs/heads/main", "refs/heads/weekly"))
	slow.release <- struct{}{}
	if refs := <-slow.started; !slices.Equal(refs, []string{"refs/heads/weekly", "refs/heads/main"}) {
		t.Fatal("pending notices were not merged into one", refs)
	}
	<-slow.ctxs
	slow.release <- struct{}{}
}

// A fetch that learns a forge move returns while the reconciliation it
// started is still running.
func TestSlowReconciliationDoesNotDelayAFetch(t *testing.T) {
	slow := &slowReconcile{started: make(chan []string, 1), release: make(chan struct{}), ctxs: make(chan context.Context, 1)}
	notices := newRefNotices(t.Context(), slow.run, func(error) {})
	defer func() { _ = notices.close(t.Context()) }()
	f := gittest.New(t)
	cache, err := gitcache.New(t.Context(), gitcache.Config{Root: filepath.Join(f.Root, "cache"), Scratch: filepath.Join(f.Root, "scratch"), Binary: f.Binary,
		LiveOwners: func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil },
		Upstream: func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{RepositoryID: 1, URL: f.Bare, DefaultBranch: "refs/heads/main"}, nil
		}, RefChanged: notices.changed})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err = cache.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Work, "push", f.Bare, "HEAD:refs/heads/nightly")
	gittest.ExpireRefresh(t, f.Store())
	var out bytes.Buffer
	if err = cache.Upload(t.Context(), gittest.Repository, gitcache.ServiceIO{Input: io.NopCloser(strings.NewReader("0000")), Output: &out}); err != nil {
		t.Fatal(err)
	}
	// Upload returned; its reconciliation is running and not released yet.
	if refs := <-slow.started; !slices.Contains(refs, "refs/heads/nightly") || !strings.Contains(out.String(), "refs/heads/nightly") {
		t.Fatal("the fetch did not hand its forge move over", refs, out.String())
	}
	<-slow.ctxs
	slow.release <- struct{}{}
}

// Notices racing the shutdown never start a worker the shutdown does not wait
// for (run with -race; the reviewer's count is 50).
func TestRefNoticesRacingClose(t *testing.T) {
	notices := newRefNotices(t.Context(), func(ctx context.Context, _ string, _ []string) error { return ctx.Err() }, func(error) {})
	var readers sync.WaitGroup
	for i := range 8 {
		readers.Go(func() {
			for range 50 {
				notices.changed(t.Context(), fmt.Sprintf("origin/team/repo-%d", i), moved("refs/heads/main"))
			}
		})
	}
	if err := notices.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	notices.mu.Lock()
	defer notices.mu.Unlock()
	if len(notices.running) != 0 {
		t.Fatal("a worker outlived close", notices.running)
	}
}

// Shutdown ends a reconciliation in flight and waits for its worker; later
// notices start none.
func TestRefNoticeEndsWithTheServer(t *testing.T) {
	slow := &slowReconcile{started: make(chan []string, 1), release: make(chan struct{}), ctxs: make(chan context.Context, 1)}
	ended := make(chan error, 1)
	notices := newRefNotices(t.Context(), slow.run, func(err error) { ended <- err })
	notices.changed(t.Context(), "origin/team/widget", moved("refs/heads/nightly"))
	<-slow.started
	<-slow.ctxs
	if err := notices.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-ended; err == nil {
		t.Fatal("the reconciliation outlived the server")
	}
	notices.changed(t.Context(), "origin/team/widget", moved("refs/heads/weekly"))
	select {
	case refs := <-slow.started:
		t.Fatal("a notice after shutdown reconciled", refs)
	default:
	}
}

// settled returns at once for a repository with nothing queued, waits for a
// queued reconciliation to end, and gives up with its caller's context.
func TestRefNoticeSettledWaitsForTheRepositorysReconciliation(t *testing.T) {
	slow := &slowReconcile{started: make(chan []string, 4), release: make(chan struct{}), ctxs: make(chan context.Context, 4)}
	notices := newRefNotices(t.Context(), slow.run, func(error) {})
	defer func() { _ = notices.close(t.Context()) }()
	if err := notices.settled(t.Context(), "origin/team/widget"); err != nil {
		t.Fatal("an idle repository did not settle", err)
	}
	notices.changed(t.Context(), "origin/team/widget", moved("refs/heads/weekly"))
	<-slow.started
	<-slow.ctxs
	gone, leave := context.WithCancel(t.Context())
	leave()
	if err := notices.settled(gone, "origin/team/widget"); err == nil {
		t.Fatal("settled before the reconciliation ended")
	}
	result := make(chan error, 1)
	go func() { result <- notices.settled(t.Context(), "origin/team/widget") }()
	close(slow.release)
	if err := <-result; err != nil {
		t.Fatal("the ended reconciliation did not settle its waiter", err)
	}
}
