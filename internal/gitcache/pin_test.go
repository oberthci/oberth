package gitcache_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestPinUsesFreshProducerHeadAndDistinguishesAbsenceFromOutage(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	registered, down := true, false
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.Upstream = func(context.Context, string) (gitcache.Remote, error) {
			if down {
				return gitcache.Remote{}, io.ErrUnexpectedEOF
			}
			if !registered {
				return gitcache.Remote{}, nil
			}
			return gitcache.Remote{RepositoryID: 17, URL: f.Bare, DefaultBranch: "refs/heads/main"}, nil
		}
	})
	at := sourcecontract.Revision{Repository: gittest.Repository, Commit: f.Commit}
	assertPin(t, c, at, pinWant{17, true})
	f.Write(t, "producer", []byte("fresh"))
	next := f.CommitFile(t, "fresh")
	f.Git(t, f.Work, "push", f.Bare, "main")
	at.Commit = next
	assertPin(t, c, at, pinWant{17, true})
	f.Git(t, f.Bare, "update-ref", "refs/heads/main", f.Commit)
	assertPin(t, c, at, pinWant{17, false})
	at.Commit = strings.Repeat("f", 40)
	assertPin(t, c, at, pinWant{17, false})
	registered = false
	assertPin(t, c, at, pinWant{0, false})
	down = true
	if _, _, err := c.Pin(t.Context(), at); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("outage became absence", err)
	}
	registered, down = true, false
	if err := os.Rename(f.Bare, f.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Pin(t.Context(), at); err == nil {
		t.Fatal("Git outage became unreachable")
	}
}

type pinWant struct {
	id        int64
	reachable bool
}

func assertPin(t *testing.T, c *gitcache.Cache, at sourcecontract.Revision, want pinWant) {
	t.Helper()
	id, reachable, err := c.Pin(t.Context(), at)
	if err != nil || id != want.id || reachable != want.reachable {
		t.Fatalf("Pin %d/%v %v; want %d/%v", id, reachable, err, want.id, want.reachable)
	}
}

func TestReceivePinReadsQuarantineWithoutImportAndExpiresForOtherProducers(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	var c *gitcache.Cache
	var reader *gitcache.ReceiveSource
	var next string
	c = configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.Upstream = func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{RepositoryID: 17, URL: f.Bare, DefaultBranch: "refs/heads/main"}, nil
		}
		cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
			reader = s
			return checkPinQuarantine(ctx, f, quarantineCheck{cache: c, source: s, commit: next})
		}
	})
	producer := "origin/team/producer"
	if err := c.Init(t.Context(), producer); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "file.txt", []byte("pushed producer"))
	next = f.CommitFile(t, "producer")
	f.Git(t, f.Work, "push", f.Bare, "main")
	updates := []gitcache.RefUpdate{{Ref: "refs/heads/feature", NewSHA: next}}
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveBatchInput(t, f, updates, nil)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err := c.Receive(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if reader == nil {
		t.Fatal("reader not used")
	}
	if _, _, err := reader.Pin(
		t.Context(),
		sourcecontract.Revision{Repository: producer, Commit: f.Commit},
	); !errors.Is(err, context.Canceled) {
		t.Fatal("reader outlived gate", err)
	}
	if _, err := reader.Checkout(t.Context(), producer, sourcecontract.Ref{}); !errors.Is(err, context.Canceled) {
		t.Fatal("default reader outlived gate", err)
	}
	if refs := f.Git(
		t,
		filepath.Join(f.Root, "cache", "origin", "team", "producer.git"),
		"for-each-ref",
		"--format=%(refname)",
		"refs/oberth/keep/",
	); refs != "" {
		t.Fatal("Pin kept producer objects", refs)
	}
	var _ gitport.Git = c
}

func refSnapshot(ctx context.Context, f *gittest.Fixture) (string, error) {
	cmd := exec.CommandContext(ctx, f.Binary, "for-each-ref", "--format=%(refname):%(objectname)")
	cmd.Dir = f.Store()
	data, err := cmd.Output()
	return string(data), err
}
func checkPinQuarantine(ctx context.Context, f *gittest.Fixture, q quarantineCheck) error {
	at := sourcecontract.Revision{Repository: gittest.Repository, Commit: q.commit}
	if _, err := q.cache.ReadFile(ctx, at, "file.txt"); err == nil {
		return errors.New("pushed source already imported")
	}
	before, err := refSnapshot(ctx, f)
	if err != nil {
		return err
	}
	id, reachable, err := q.source.Pin(ctx, at)
	if err != nil {
		return err
	}
	if id != 17 || !reachable {
		return errors.New("quarantined producer was not reachable")
	}
	head, err := q.source.Checkout(ctx, gittest.Repository, sourcecontract.Ref{})
	if err != nil {
		return err
	}
	if head.Ref.CommitSHA != q.commit || head.Ref != head.DefaultBranch {
		return errors.New("quarantine default selector used stale head")
	}
	if _, err = q.cache.ReadFile(ctx, at, "file.txt"); err == nil {
		return errors.New("Pin imported quarantine")
	}
	after, err := refSnapshot(ctx, f)
	if err != nil {
		return err
	}
	if after != before {
		return errors.New("Pin moved refs")
	}
	return nil
}

func TestReleaseDefaultAncestorReturnsTheHeadItChecked(t *testing.T) {
	f := gittest.New(t)
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.Upstream = func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{RepositoryID: 17, URL: f.Bare, DefaultBranch: "refs/heads/main"}, nil
		}
	})
	f.Write(t, "later", []byte("next head"))
	next := f.CommitFile(t, "next head")
	f.Git(t, f.Work, "push", f.Bare, "main")
	head, reachable, err := c.DefaultAncestor(t.Context(), sourcecontract.Revision{Repository: gittest.Repository, Commit: f.Commit})
	if err != nil || !reachable || head.Name != "refs/heads/main" || head.CommitSHA != next || head.ObjectSHA != next {
		t.Fatal(head, reachable, err)
	}
	f.Git(t, f.Bare, "update-ref", "refs/heads/main", f.Commit)
	if head.CommitSHA != next {
		t.Fatal("recorded head changed")
	}
	_, reachable, err = c.DefaultAncestor(t.Context(), sourcecontract.Revision{Repository: gittest.Repository, Commit: next})
	if err != nil || reachable {
		t.Fatal("outside default history accepted", reachable, err)
	}
}
