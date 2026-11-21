package gitcache_test

import (
	"context"
	"k8s.io/utils/clock"

	"fmt"
	"io"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"

	"strings"
	"sync"

	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReviewSecondConcurrentGit(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.LiveOwners = func(context.Context, string) (gitcache.OwnerPage, error) {
		return gitcache.OwnerPage{Owners: []gitcache.Owner{{ID: "run", Bindings: f.Bindings(f.Commit)}}}, nil
	}
	c := reviewNew(t, cfg)
	if e := c.Init(t.Context(), gittest.Repository); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reviewConcurrentWork(t.Context(), concurrentWork{c, f, i}, errs)

		}(i)
	}
	for j := 0; j < 5; j++ {
		e := c.Receive(
			t.Context(),
			gitcache.ReceiveRequest{
				Repository:  gittest.Repository,
				Actor:       "a",
				Fingerprint: "f",
				Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
				Input: receiveRefInput(
					t,
					f,
					gitcache.RefUpdate{Ref: fmt.Sprintf("refs/heads/topic%d", j), NewSHA: f.Commit},
				),
				Output: io.Discard,
				Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
					return nil, nil
				},
			},
		)
		if e != nil {
			t.Error(e)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if !strings.Contains(e.Error(), "Git input is invalid") {
			t.Log("concurrent op refusal:", e)
		}
	}
	f.Git(t, f.Store(), "fsck", "--full")
	if len(reviewLocks(t, f)) > 0 {
		t.Error("dangling locks")
	}
	refs, e := c.ListRefs(t.Context(), gittest.Repository)
	if e != nil || len(refs) != 6 {
		t.Errorf("lost accepted ref %d %v", len(refs), e)
	}
}

func TestReviewSecondLargePublicRefs(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, nil)
	var packed strings.Builder
	packed.WriteString("# pack-refs with: peeled fully-peeled sorted\n")
	for i := 0; i < 4999; i++ {
		fmt.Fprintf(&packed, "%s refs/heads/r%05d\n", f.Commit, i)
	}
	if e := os.WriteFile(filepath.Join(f.Store(), "packed-refs"), []byte(packed.String()), 0600); e != nil {
		t.Fatal(e)
	}
	start := (clock.RealClock{}).Now()
	refs, e := c.ListRefs(t.Context(), gittest.Repository)
	t.Logf("50000 public refs ListRefs count=%d err=%v duration=%s", len(refs), e, (clock.RealClock{}).Now().Sub(start))
	stage, e := c.Stage(t.Context(), f.Binding())
	if e != nil {
		t.Fatal("exact snapshot failed with unrelated refs", e)
	}
	_ = stage.Body.Close()
	cfg := reviewConfig(t, f)
	_ = c.Close()
	c = reviewNew(t, cfg)
	for _, name := range []string{"r04998", "r00000"} {
		e = c.Receive(
			t.Context(),
			gitcache.ReceiveRequest{
				Repository:  gittest.Repository,
				Actor:       "a",
				Fingerprint: "f",
				Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
				Input:       receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/" + name, OldSHA: f.Commit}),
				Output:      io.Discard,
				Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
					return nil, nil
				},
			},
		)
		t.Logf("delete %s from oversized repo: %v", name, e)
		if e != nil {
			t.Error("bounded deletion cannot reduce an oversized public namespace")
		}
	}
}

func TestReviewSecondLargeKeepGC(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	c := reviewNew(t, cfg)
	if e := c.Init(t.Context(), gittest.Repository); e != nil {
		t.Fatal(e)
	}
	var packed strings.Builder
	packed.WriteString("# pack-refs with: peeled fully-peeled sorted\n")
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&packed, "%s refs/oberth/keep/run%05d/%s\n", f.Commit, i, f.Commit)
	}
	if e := os.WriteFile(filepath.Join(f.Store(), "packed-refs"), []byte(packed.String()), 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := (clock.RealClock{}).Now()
	e := c.GC(ctx)
	log, _ := os.ReadFile(filepath.Join(f.Root, "commands.jsonl"))
	if inventories := strings.Count(string(log), `"refs/oberth/keep/"`); inventories != 1 {
		t.Fatalf("keep inventories=%d, want one for the whole sweep", inventories)
	}
	t.Logf(
		"GC 50000 owners after %s: %v; native invocations=%d",
		(clock.RealClock{}).Now().Sub(start),
		e,
		strings.Count(string(log), "\n"),
	)
	all := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/")
	remaining := len(strings.Fields(all))
	t.Logf("remaining owners=%d removed=%d", remaining, 50000-remaining)
	if e != nil || remaining != 0 {
		t.Fatalf("GC did not finish linear sweep: %v remaining=%d", e, remaining)
	}
}

func TestReviewSecondArchiveCancellation(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, nil)
	cmd := exec.Command(f.Binary, "hash-object", "-w", "--stdin")
	cmd.Dir = f.Store()
	cmd.Stdin = io.LimitReader(mathrand.New(mathrand.NewSource(42)), 256<<20)
	if _, e := cmd.Output(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	var body io.ReadCloser
	var err error
	go func() { body, err = c.Archive(ctx, gittest.Repository); close(done) }()
	waitArchiveCopy(t, f.Root, done)
	start := (clock.RealClock{}).Now()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("archive did not stop within one second")
	}
	t.Logf("archive after cancellation=%s err=%v", (clock.RealClock{}).Now().Sub(start), err)
	if body != nil {
		_ = body.Close()
	}
	if err == nil {
		t.Error("Archive ignored cancellation during private archive construction")
	}
	files, globErr := filepath.Glob(filepath.Join(f.Root, "scratch", "oberth-export-archive-*"))
	if globErr != nil || len(files) != 0 {
		t.Fatal("partial archive survived cancellation", files, globErr)
	}
}

func TestReviewSecondNativeRefRefusals(t *testing.T) {
	for _, name := range []string{"default-delete", "reserved-delete", "tag-replace", "default-rewind"} {
		t.Run(name, func(t *testing.T) {
			f := gittest.New(t)
			cfg := reviewConfig(t, f)
			cfg.ReservedBranches = []string{"release/*"}
			c := reviewNew(t, cfg)
			if e := c.Init(t.Context(), gittest.Repository); e != nil {
				t.Fatal(e)
			}
			f.Write(t, "next", []byte("next"))
			next := f.CommitFile(t, "next")
			f.Git(t, f.Store(), "fetch", f.Work, next)
			u := gitcache.RefUpdate{Ref: "refs/heads/main", OldSHA: f.Commit}
			switch name {
			case "reserved-delete":
				u.Ref = "refs/heads/release/test"
				f.Git(t, f.Store(), "update-ref", u.Ref, f.Commit)
			case "tag-replace":
				u.Ref = "refs/tags/release"
				u.NewSHA = next
				f.Git(t, f.Store(), "update-ref", u.Ref, f.Commit)
			case "default-rewind":
				f.Git(t, f.Store(), "update-ref", u.Ref, next)
				u.OldSHA = next
				u.NewSHA = f.Commit
			}
			delivered := false
			e := c.Receive(
				t.Context(),
				gitcache.ReceiveRequest{
					Repository:   gittest.Repository,
					Actor:        "operator",
					Fingerprint:  "f",
					AllowDefault: true,
					Admission:    gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
					Input:        receiveRefInput(t, f, u),
					Output:       io.Discard,
					Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
						delivered = true
						return nil, nil
					},
				},
			)
			if e == nil || delivered {
				t.Errorf("guard failed: %v delivered=%v", e, delivered)
			}
			if got := f.Git(t, f.Store(), "rev-parse", u.Ref); got != u.OldSHA {
				t.Error("refusal mutated ref", got)
			}
		})
	}
}

type concurrentWork struct {
	cache   *gitcache.Cache
	fixture *gittest.Fixture
	kind    int
}

func reviewConcurrentWork(ctx context.Context, work concurrentWork, errs chan<- error) {
	c, f, kind := work.cache, work.fixture, work.kind
	for range 5 {
		var err error
		switch kind {
		case 0:
			err = c.Keep(ctx, "run", f.Bindings(f.Commit))
		case 1:
			err = c.GC(ctx)
		case 2:
			err = c.ForceFetch(ctx, gittest.Repository)
		}
		if err != nil {
			errs <- err
		}
	}
}

func archiveCopyStarted(root string) bool {
	files, _ := filepath.Glob(filepath.Join(root, "scratch", "oberth-export-archive-*", "repository.tgz"))
	if len(files) == 0 {
		return false
	}
	info, err := os.Stat(files[0])
	return err == nil && info.Size() > 64<<10
}

func waitArchiveCopy(t *testing.T, root string, done <-chan struct{}) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timeout := time.After(10 * time.Second)
	for !archiveCopyStarted(root) {
		select {
		case <-done:
			t.Fatal("archive finished before cancel point")
		case <-timeout:
			t.Fatal("archive did not start")
		case <-tick.C:
		}
	}
}
