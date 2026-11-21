package gitcache_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestKeepRejectsNonCommitRoot(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	blob := f.Git(t, f.Work, "rev-parse", f.Commit+":file.txt")
	if err := c.Keep(t.Context(), "run", f.Bindings(blob)); err == nil {
		t.Fatal("non-commit root was retained")
	}
}
func TestKeepTransactionDoesNotPublishPrefixOnLockFailure(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Write(t, "other", []byte("commit"))
	next := f.CommitFile(t, "other")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	roots := []string{f.Commit, next}
	sort.Strings(roots)
	dir := filepath.Join(f.Store(), "refs", "oberth", "keep", "run")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, roots[1]+".lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	err := c.Keep(t.Context(), "run", f.Bindings(roots[0], roots[1]))
	if err == nil {
		t.Fatal("locked root was accepted")
	}
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/run/"); got != "" {
		t.Fatalf("partial retention: %s", got)
	}
}
func TestOwnerLengthAccepts128Refuses129(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	roots := f.Bindings(f.Commit)
	if err := c.Keep(t.Context(), strings.Repeat("a", 128), roots); err != nil {
		t.Fatal(err)
	}
	if err := c.Keep(t.Context(), strings.Repeat("a", 129), roots); err == nil {
		t.Fatal("129-byte owner accepted")
	}
}
func TestReconcileOwnerPageAccepts128Refuses129(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	count := 128
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) {
		owners := make([]gitcache.Owner, count)
		for i := range owners {
			owners[i] = gitcache.Owner{ID: "run", Bindings: f.Bindings(f.Commit)}
		}
		return gitcache.OwnerPage{Owners: owners}, nil
	})
	if err := c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	count++
	if err := c.GC(t.Context()); err == nil {
		t.Fatal("oversized owner page accepted")
	}
}
func TestRecordedMissingTagIsTerminalRefusal(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Git(t, f.Work, "tag", "-a", "tag", "-m", "tag")
	f.Git(t, f.Work, "push", f.Bare, "--tags")
	b := f.Binding()
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) {
		return gitcache.OwnerPage{Owners: []gitcache.Owner{{ID: "run", Bindings: []sourcecontract.Binding{b}}}}, nil
	})
	tag, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/tags/tag")
	if err != nil {
		t.Fatal(err)
	}
	b.Export.Tags = []sourcecontract.Ref{tag}
	f.Git(t, f.Store(), "update-ref", "-d", "refs/tags/tag")
	f.Git(t, f.Store(), "update-ref", "-d", "refs/oberth/upstream/tags/tag")
	if err = c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = c.Stage(t.Context(), b)
	var failure *sourcedelivery.Failure
	if !errors.As(err, &failure) || failure.Code != "gone" {
		t.Fatalf("missing recorded tag: %v", err)
	}
}

func TestReceiveInspectionAndExactAbandonment(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	f.Write(t, "next", []byte("change"))
	next := f.CommitFile(t, "change")
	err := c.Receive(
		t.Context(),
		gitcache.ReceiveRequest{
			AllowDefault: true,
			Repository:   gittest.Repository,
			Actor:        "operator",
			Fingerprint:  "key",
			Admission:    gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
			Input:        receiveInput(t, f, f.Commit, next),
			Output:       io.Discard,
		},
	)
	if err == nil {
		t.Fatal("undelivered receive lost")
	}
	pending, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil || pending.State != "ready" {
		t.Fatalf("pending %+v %v", pending, err)
	}
	if err = c.AbandonReceive(t.Context(), gittest.Repository, "another-event"); err == nil {
		t.Fatal("abandoned different event")
	}
	if err = c.AbandonReceive(t.Context(), gittest.Repository, pending.Event.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.Git(t, f.Store(), "rev-parse", "refs/heads/main"); got != next {
		t.Fatal("abandonment changed accepted refs")
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs != "" {
		t.Fatal("abandoned pins retained", refs)
	}
}

func TestUnkeptPrunedCommitReturnsGone(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil })
	f.Write(t, "unkept", []byte("source"))
	commit := f.CommitFile(t, "unkept")
	f.Git(t, f.Store(), "fetch", f.Work, commit)
	b := f.Binding()
	b.Revision.Commit = commit
	b.Export = sourcecontract.Export{}
	stage, err := c.Stage(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	_ = stage.Body.Close()
	if err = c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = c.Stage(t.Context(), b)
	var failure *sourcedelivery.Failure
	if !errors.As(err, &failure) || failure.Code != "gone" {
		t.Fatal("pruned commit not reported as gone", err)
	}
}
