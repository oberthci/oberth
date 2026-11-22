package gitcache_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// transferring is the progress line a held fetch prints first, as a forge
// that answers and sends much does.
const transferring = "Receiving objects:   1% (1/100)"

// slowFetch holds every refresh fetch from the fixture's forge at a gate the
// test opens, after printing progress (a forge sending much) or nothing (a
// forge that keeps silent), so the fetch keeps running past its caller's
// deadline. Fetches of other forges and single-object fetches pass.
// The log counts the held refresh fetches ("refresh") and the single-object
// forge fetches ("object"). The gate stays open for writing until cleanup, so
// a release never waits for a reader and a fetch that never came only leaves
// its line unread.
type slowFetch struct {
	gate *os.File
	log  string
}

func holdFetches(t *testing.T, f *gittest.Fixture, cfg *gitcache.Config, progress string) *slowFetch {
	t.Helper()
	dir := t.TempDir()
	gate, log, script := filepath.Join(dir, "gate"), filepath.Join(dir, "fetches"), filepath.Join(dir, "git")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`#!/bin/sh
held= fetch= refspec=
for a do
  [ "$a" = fetch ] && fetch=1
  [ "$a" = '%s' ] && held=1
  [ "$a" = '+refs/heads/*:refs/heads/*' ] && refspec=1
done
if [ -n "$fetch" ] && [ -n "$held" ]; then
  if [ -n "$refspec" ]; then
    echo refresh >> '%s'
    [ -n '%s' ] && echo '%s' >&2
    read _ < '%s'
  else
    echo object >> '%s'
  fi
fi
exec '%s' "$@"
`, f.Bare, log, progress, progress, gate, log, f.Binary)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = script
	w, err := os.OpenFile(gate, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return &slowFetch{gate: w, log: log}
}

func (s *slowFetch) release(t *testing.T) {
	t.Helper()
	if _, err := s.gate.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
}

func (s *slowFetch) count(t *testing.T, kind string) int {
	t.Helper()
	data, err := os.ReadFile(s.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Count(string(data), kind+"\n")
}

func shortContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func checkStillUpdating(t *testing.T, err error) {
	t.Helper()
	var updating *gitport.FetchUnfinishedError
	if !errors.As(err, &updating) || publicerr.Classify(err).Reason() != gitport.FetchUnfinishedReason || publicerr.Permanent(err) {
		t.Fatal("a fetch past its caller's wait was not reported as still updating", err)
	}
	if strings.Contains(err.Error(), gitport.ForgeReason) {
		t.Fatal("an answering forge was reported unreachable", err)
	}
}

// advance gives the forge a commit the cache lacks and makes the cache's last
// refresh stale.
func advance(t *testing.T, f *gittest.Fixture, now *time.Time) string {
	t.Helper()
	f.Write(t, "large", []byte("large forge change"))
	next := f.CommitFile(t, "large forge change")
	f.Git(t, f.Work, "push", f.Bare, "main")
	*now = now.Add(gitcache.RefreshInterval)
	return next
}

type fetchFixture struct {
	f     *gittest.Fixture
	cfg   gitcache.Config
	now   time.Time
	slow  *slowFetch
	mu    sync.Mutex
	moves [][]gitcache.RefUpdate
}

func newFetchFixture(t *testing.T, progress string) *fetchFixture {
	t.Helper()
	x := &fetchFixture{f: gittest.New(t), now: time.Unix(2000000000, 0)}
	x.cfg = reviewConfig(t, x.f)
	x.cfg.Now = func() time.Time {
		x.mu.Lock()
		defer x.mu.Unlock()
		return x.now
	}
	x.cfg.RefChanged = func(_ context.Context, repo string, updates []gitcache.RefUpdate) {
		x.mu.Lock()
		defer x.mu.Unlock()
		if repo == gittest.Repository {
			x.moves = append(x.moves, updates)
		}
	}
	x.slow = holdFetches(t, x.f, &x.cfg, progress)
	return x
}

func (x *fetchFixture) start(t *testing.T) *gitcache.Cache {
	t.Helper()
	c := reviewNew(t, x.cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	return c
}

func (x *fetchFixture) advance(t *testing.T) string {
	t.Helper()
	x.mu.Lock()
	defer x.mu.Unlock()
	return advance(t, x.f, &x.now)
}

// A refresh fetch that needs longer than its callers wait keeps running:
// both callers are told the forge copy is still updating, they share one
// fetch, the fetch announces its moves once after both left, and the next
// caller reads the new head without fetching again.
func TestLargeFetchFinishesAfterItsCallersLeave(t *testing.T) {
	x := newFetchFixture(t, transferring)
	c := x.start(t)
	next := x.advance(t)
	for range 2 {
		_, err := c.RemoteRef(shortContext(t), gittest.Repository, "refs/heads/main")
		checkStillUpdating(t, err)
	}
	x.slow.release(t)
	gitcache.WaitBackground(c)
	if got := x.slow.count(t, "refresh"); got != 1 {
		t.Fatal("callers did not share one fetch", got)
	}
	x.mu.Lock()
	moves := slices.Clone(x.moves)
	x.mu.Unlock()
	if len(moves) != 1 || !slices.ContainsFunc(moves[0], func(u gitcache.RefUpdate) bool { return u.Ref == "refs/heads/main" && u.NewSHA == next }) {
		t.Fatal("the finished fetch did not announce its moves once", moves)
	}
	head, err := c.RemoteRef(t.Context(), gittest.Repository, "refs/heads/main")
	if err != nil || head.CommitSHA != next || x.slow.count(t, "refresh") != 1 || x.slow.count(t, "object") != 0 {
		t.Fatal("the next caller did not read the fetched head from the cache", head, err)
	}
}

// A read served while the fetch runs says the forge copy is updating, not
// that the forge did not answer, and the fetch continues.
func TestReadDuringLargeFetchServesTheCacheAndSaysWhy(t *testing.T) {
	x := newFetchFixture(t, transferring)
	c := x.start(t)
	x.advance(t)
	gitcache.SetRefreshWait(c, 50*time.Millisecond)
	var notice strings.Builder
	if err := c.RefreshRead(t.Context(), gittest.Repository, &notice); err != nil {
		t.Fatal("a read during the fetch was not served from the cache", err)
	}
	if !strings.HasPrefix(notice.String(), gitport.ForgeUpdatingNotice) || strings.Contains(notice.String(), gitport.ForgeCacheNotice) {
		t.Fatal("the cache notice does not say the forge copy is updating", notice.String())
	}
	x.slow.release(t)
	gitcache.WaitBackground(c)
}

// A forge that sends no pack past the caller's wait (silent, or stalled after
// an error) is still reported as not answering, and the read is served from
// the cache as before.
func TestStalledForgeIsReportedAsNotAnswering(t *testing.T) {
	for _, output := range []string{"", "fatal: the remote end hung up unexpectedly", "remote: fatal: upload-pack is not allowed here"} {
		t.Run(output, func(t *testing.T) {
			x := newFetchFixture(t, output)
			c := x.start(t)
			x.advance(t)
			gitcache.SetRefreshWait(c, 50*time.Millisecond)
			var notice strings.Builder
			if err := c.RefreshRead(t.Context(), gittest.Repository, &notice); err != nil {
				t.Fatal("a read during a stalled fetch was not served from the cache", err)
			}
			if !strings.HasPrefix(notice.String(), gitport.ForgeCacheNotice) {
				t.Fatal("a stalled forge was reported as updating", notice.String())
			}
			_, err := c.RemoteRef(shortContext(t), gittest.Repository, "refs/heads/main")
			var outage *gitport.UnavailableError
			if !errors.As(err, &outage) || outage.Op != "forge" {
				t.Fatal("a stalled forge was not reported as not answering", err)
			}
			x.slow.release(t)
			gitcache.WaitBackground(c)
		})
	}
}

// A fetch that keeps sending past the background bound ends there and its
// failure is recorded, so the next readers are told the forge could not be
// reached instead of "still updating" again, and no new transfer starts
// within the interval.
func TestFetchEndingAtItsBoundIsRecorded(t *testing.T) {
	x := newFetchFixture(t, transferring)
	c := x.start(t)
	x.advance(t)
	gitcache.SetBackgroundBound(c, 300*time.Millisecond)
	_, err := c.RemoteRef(shortContext(t), gittest.Repository, "refs/heads/main")
	checkStillUpdating(t, err)
	gitcache.WaitBackground(c)
	_, err = c.RemoteRef(shortContext(t), gittest.Repository, "refs/heads/main")
	var outage *gitport.UnavailableError
	if !errors.As(err, &outage) || outage.Op != "forge" || x.slow.count(t, "refresh") != 1 {
		t.Fatal("a fetch ended at its bound was not recorded as a forge failure", err, x.slow.count(t, "refresh"))
	}
}

// A refresh fetch that stops moving ends at the idle limit as before, its
// failure is recorded from its own outcome, and nothing moves.
func TestStalledFetchEndsAtTheIdleLimit(t *testing.T) {
	x := newFetchFixture(t, transferring)
	c := x.start(t)
	x.advance(t)
	gitcache.DeltaSetNativeIdle(c, 200*time.Millisecond)
	_, err := c.RemoteRef(shortContext(t), gittest.Repository, "refs/heads/main")
	checkStillUpdating(t, err)
	gitcache.WaitBackground(c)
	_, err = c.RemoteRef(t.Context(), gittest.Repository, "refs/heads/main")
	var outage *gitport.UnavailableError
	if !errors.As(err, &outage) || outage.Op != "forge" {
		t.Fatal("a stalled fetch did not end as a recorded forge failure", err)
	}
	if ref, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main"); err != nil || ref.CommitSHA != x.f.Commit {
		t.Fatal("a stalled fetch moved the cache", ref, err)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.moves) != 0 {
		t.Fatal("a stalled fetch announced moves", x.moves)
	}
}

// A pin's producer head the cache lacks arrives with the producer's refresh
// fetch, which continues past the admission; a head the cache has needs no
// forge fetch at all.
func TestPinOfAHeadTheCacheLacksWaitsForTheRefresh(t *testing.T) {
	x := newFetchFixture(t, transferring)
	c := x.start(t)
	at := sourcecontract.Revision{Repository: gittest.Repository, Commit: x.f.Commit}
	if _, reachable, err := c.Pin(t.Context(), at); err != nil || !reachable || x.slow.count(t, "object") != 0 {
		t.Fatal("a head the cache has was fetched again", reachable, err, x.slow.count(t, "object"))
	}
	x.advance(t)
	gitcache.SetHeadWait(c, 50*time.Millisecond)
	_, _, err := c.Pin(t.Context(), at)
	checkStillUpdating(t, err)
	x.slow.release(t)
	gitcache.WaitBackground(c)
	if _, reachable, err := c.Pin(t.Context(), at); err != nil || !reachable {
		t.Fatal("the pin was not admitted after the refresh", reachable, err)
	}
	if x.slow.count(t, "refresh") != 1 || x.slow.count(t, "object") != 0 {
		t.Fatal("the producer head was not brought by one refresh", x.slow.count(t, "refresh"), x.slow.count(t, "object"))
	}
}

// A pin whose producer head arrives with a refresh that the pin waited for
// tells the cache's readers what moved, so the producer's schedules follow.
func TestPinRefreshAnnouncesTheProducersMoves(t *testing.T) {
	x := newFetchFixture(t, transferring)
	c := x.start(t)
	next := x.advance(t)
	x.slow.release(t)
	at := sourcecontract.Revision{Repository: gittest.Repository, Commit: x.f.Commit}
	if _, reachable, err := c.Pin(t.Context(), at); err != nil || !reachable {
		t.Fatal("the pin was not admitted", reachable, err)
	}
	gitcache.WaitBackground(c)
	x.mu.Lock()
	moves := slices.Clone(x.moves)
	x.mu.Unlock()
	if len(moves) != 1 || !slices.ContainsFunc(moves[0], func(u gitcache.RefUpdate) bool { return u.Ref == "refs/heads/main" && u.NewSHA == next }) {
		t.Fatal("the pin's refresh did not announce the producer's moves once", moves)
	}
}

// A fetch that outlives an upstream removed and added again under another
// address imports and announces nothing from the old forge; the next refresh
// reads the new address.
func TestFetchOfAMovedUpstreamImportsNothing(t *testing.T) {
	x := newFetchFixture(t, transferring)
	moved := gittest.New(t)
	var mu sync.Mutex
	url := x.f.Bare
	x.cfg.Upstream = func(context.Context, string) (gitcache.Remote, error) {
		mu.Lock()
		defer mu.Unlock()
		return gitcache.Remote{URL: url, DefaultBranch: "refs/heads/main", RepositoryID: 1}, nil
	}
	c := x.start(t)
	x.advance(t)
	_, err := c.RemoteRef(shortContext(t), gittest.Repository, "refs/heads/main")
	checkStillUpdating(t, err)
	mu.Lock()
	url = moved.Bare
	mu.Unlock()
	x.slow.release(t)
	gitcache.WaitBackground(c)
	if ref, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main"); err != nil || ref.CommitSHA != x.f.Commit {
		t.Fatal("the old forge's fetch moved the cache", ref, err)
	}
	x.mu.Lock()
	announced := len(x.moves)
	x.mu.Unlock()
	if announced != 0 {
		t.Fatal("the old forge's fetch was announced", announced)
	}
	head, err := c.RemoteRef(t.Context(), gittest.Repository, "refs/heads/main")
	if err != nil || head.CommitSHA != moved.Commit {
		t.Fatal("the next refresh did not read the new address", head, err)
	}
}

// A fetch started inside a push (a read of the pushed repository's default
// head) runs on the store, never in the push's quarantine: it outlives the
// push, whose quarantine is gone by then, and still lands.
func TestFetchStartedInAPushOutlivesItsQuarantine(t *testing.T) {
	x := newFetchFixture(t, transferring)
	x.cfg.PreReceive = func(ctx context.Context, e gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		_, err := s.Checkout(ctx, e.Repository, sourcecontract.Ref{})
		return err
	}
	c := x.start(t)
	gitcache.SetRefreshWait(c, 100*time.Millisecond)
	next := x.advance(t)
	var diagnostics strings.Builder
	r := gateReceive(x.f)
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, x.f, gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: x.f.Commit})
	r.Output = io.Discard
	r.Diagnostics = &diagnostics
	if err := c.Receive(t.Context(), r); err == nil || !strings.Contains(diagnostics.String(), gitport.FetchUnfinishedReason) {
		t.Fatalf("the push was not told the forge copy is updating: %v\n%s", err, diagnostics.String())
	}
	x.slow.release(t)
	gitcache.WaitBackground(c)
	if ref, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/main"); err != nil || ref.CommitSHA != next {
		t.Fatal("the fetch started in the push did not land after the push", ref, err)
	}
}

const otherRepository = "origin/team/other"

// pushFixture is a cache whose admission reads the forge's default head of
// the pushed repository, as admission does for a default-branch or tag push.
type pushFixture struct {
	*fetchFixture
	other *gittest.Fixture
	c     *gitcache.Cache
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	x := &pushFixture{fetchFixture: newFetchFixture(t, transferring), other: gittest.New(t)}
	upstream := x.cfg.Upstream
	x.cfg.Upstream = func(ctx context.Context, repo string) (gitcache.Remote, error) {
		if repo == otherRepository {
			return gitcache.Remote{URL: x.other.Bare, DefaultBranch: "refs/heads/main", RepositoryID: 2}, nil
		}
		return upstream(ctx, repo)
	}
	x.cfg.PreReceive = func(ctx context.Context, e gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
		_, err := x.c.RemoteRef(ctx, e.Repository, "refs/heads/main")
		return err
	}
	x.c = x.start(t)
	if err := x.c.Init(t.Context(), otherRepository); err != nil {
		t.Fatal(err)
	}
	gitcache.SetRefreshWait(x.c, 100*time.Millisecond)
	return x
}

func (x *pushFixture) push(t *testing.T, f *gittest.Fixture, repo, ref string) (string, error) {
	t.Helper()
	var diagnostics strings.Builder
	r := gateReceive(f)
	r.Repository = repo
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: ref, NewSHA: f.Commit})
	r.Output = io.Discard
	r.Diagnostics = &diagnostics
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	err := x.c.Receive(t.Context(), r)
	return diagnostics.String(), err
}

// A push whose admission needs a fetch that takes longer than its wait is
// told the forge copy is still updating (not that admission is unavailable);
// a second push during the same fetch shares it; a push of another
// repository meanwhile is admitted; once the fetch ends the next push is.
func TestPushDuringLargeFetchIsToldToRetryAndTheNextIsAdmitted(t *testing.T) {
	x := newPushFixture(t)
	x.advance(t)
	for _, ref := range []string{"refs/heads/first", "refs/heads/second"} {
		diagnostics, err := x.push(t, x.f, gittest.Repository, ref)
		if err == nil || !strings.Contains(diagnostics, gitport.FetchUnfinishedReason) || strings.Contains(diagnostics, "push admission is unavailable") {
			t.Fatalf("push during the fetch was not told the forge copy is updating: %v\n%s", err, diagnostics)
		}
	}
	if diagnostics, err := x.push(t, x.other, otherRepository, "refs/heads/meanwhile"); err != nil {
		t.Fatalf("a push of another repository was refused during the fetch: %v\n%s", err, diagnostics)
	}
	x.slow.release(t)
	gitcache.WaitBackground(x.c)
	if diagnostics, err := x.push(t, x.f, gittest.Repository, "refs/heads/third"); err != nil {
		t.Fatalf("the push after the fetch was refused: %v\n%s", err, diagnostics)
	}
	if got := x.slow.count(t, "refresh"); got != 1 {
		t.Fatal("pushes did not share one fetch", got)
	}
	if ref, err := x.c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/third"); err != nil || ref.CommitSHA != x.f.Commit {
		t.Fatal("the admitted push did not move its ref", ref, err)
	}
}
