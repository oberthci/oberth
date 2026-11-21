package gitcache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

type refNotices struct {
	mu   sync.Mutex
	refs []string
}

func (n *refNotices) record(_ context.Context, _ string, updates []gitcache.RefUpdate) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, u := range updates {
		n.refs = append(n.refs, u.Ref)
	}
}

func (n *refNotices) seen(ref string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Contains(n.refs, ref)
}

// forgeBranch returns a cache whose forge gained refs/heads/nightly after the
// cache's last refresh; the cache does not have the branch yet.
func forgeBranch(t *testing.T) (*gittest.Fixture, *gitcache.Cache, *refNotices) {
	t.Helper()
	f := gittest.New(t)
	notices := &refNotices{}
	cfg := reviewConfig(t, f)
	cfg.RefChanged = notices.record
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Work, "push", f.Bare, "HEAD:refs/heads/nightly")
	if _, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/nightly"); err == nil {
		t.Fatal("fixture cache already has the forge branch")
	}
	return f, c, notices
}

// Whichever read brings a forge branch into the cache tells the cache's
// readers, so schedule reconciliation hears of a branch that reached the
// forge first.
func TestEveryRefreshTellsReadersAboutForgeBranches(t *testing.T) {
	for name, read := range map[string]func(*gitcache.Cache) error{
		"receive or upload session": func(c *gitcache.Cache) error { return c.RefreshRead(t.Context(), gittest.Repository, nil) },
		"forge ref read": func(c *gitcache.Cache) error {
			_, err := c.RemoteRef(t.Context(), gittest.Repository, "refs/heads/main")
			return err
		},
		"default-branch file": func(c *gitcache.Cache) error {
			_, _, err := c.DefaultFile(t.Context(), gittest.Repository, "file.txt")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, c, notices := forgeBranch(t)
			gittest.ExpireRefresh(t, f.Store())
			if err := read(c); err != nil {
				t.Fatal(err)
			}
			if !notices.seen("refs/heads/nightly") {
				t.Fatal("a refresh moved a public ref without telling its readers", notices.refs)
			}
		})
	}
}

// A forge that does not answer right after a refresh leaves the cached answer
// for the forced refresh too: a branch the cache lacks is absent, not an outage.
func TestResolveFreshKeepsTheCachedAnswerWhenTheForgeIsDown(t *testing.T) {
	f, c, _ := forgeBranch(t)
	if err := os.Rename(f.Bare, f.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, err := c.ResolveFresh(t.Context(), gittest.Repository, "refs/heads/weekly")
		var absent *gitport.AbsentError
		if !errors.As(err, &absent) {
			t.Fatal("a forge outage hid the cached answer", err)
		}
	}
	if ref, err := c.ResolveFresh(t.Context(), gittest.Repository, "refs/heads/main"); err != nil || ref.CommitSHA != f.Commit {
		t.Fatal("a cached branch was not read during the outage", ref, err)
	}
}

// A ref the cache lacks is looked for on the forge at most once per repository
// per freshness interval, however often it is read and through which entry:
// many reads fetch once, and a read after the interval fetches again.
func TestResolveFreshLooksForAMissingRefOncePerInterval(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	fetches := func() int {
		data, err := os.ReadFile(filepath.Join(f.Root, "commands.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), `"+refs/heads/*:refs/heads/*"`)
	}
	for _, round := range []string{"first interval", "after the interval"} {
		gittest.ExpireRefresh(t, f.Store())
		before := fetches()
		for range 5 {
			if _, err := c.ResolveFresh(t.Context(), gittest.Repository, "refs/heads/nowhere"); err == nil {
				t.Fatal("a branch that is nowhere resolved")
			}
		}
		if got := fetches() - before; got != 1 {
			t.Fatal(round, "forge fetches for five reads:", got)
		}
	}
}

// Schedule reconciliation finds a forge branch inside the freshness interval,
// and its own refresh sends no notice back to it.
func TestResolveFreshFindsAForgeBranchWithinTheFreshnessInterval(t *testing.T) {
	f, c, notices := forgeBranch(t)
	ref, err := c.ResolveFresh(t.Context(), gittest.Repository, "refs/heads/nightly")
	if err != nil || ref.CommitSHA != f.Commit {
		t.Fatal("forge branch not found", ref, err)
	}
	if len(notices.refs) != 0 {
		t.Fatal("reconciliation's own refresh notified it", notices.refs)
	}
	_, err = c.ResolveFresh(t.Context(), gittest.Repository, "refs/heads/nowhere")
	var absent *gitport.AbsentError
	if !errors.As(err, &absent) {
		t.Fatal("a branch that is nowhere was not reported absent", err)
	}
}
