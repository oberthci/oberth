package gitcache_test

import (
	"io"
	"k8s.io/utils/clock"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestStepLoadsReuseStagedArchive(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, nil)
	if repository := os.Getenv("PERF_SOURCE_REPO"); repository != "" {
		f.Git(t, f.Store(), "fetch", "--no-tags", repository, "main:refs/heads/history")
		f.Commit = f.Git(t, f.Store(), "rev-parse", "refs/heads/history")
	}
	start := (clock.RealClock{}).Now()
	first, err := c.Stage(t.Context(), f.Binding())
	if err != nil {
		t.Fatal(err)
	}
	firstTime := (clock.RealClock{}).Since(start)
	info, err := first.Body.(interface{ Stat() (os.FileInfo, error) }).Stat()
	path := first.Body.(interface{ Name() string }).Name()
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	start = (clock.RealClock{}).Now()
	second, err := c.Stage(t.Context(), f.Binding())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Body.Close() }()
	secondTime := (clock.RealClock{}).Since(start)
	next, err := second.Body.(interface{ Stat() (os.FileInfo, error) }).Stat()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first stage %v; repeated stage %v", firstTime, secondTime)
	if !os.SameFile(info, next) || second.Body.(interface{ Name() string }).Name() != path {
		t.Fatal("another step rebuilt the staged archive")
	}
	if n, err := io.Copy(io.Discard, second.Body); err != nil || n != info.Size() {
		t.Fatalf("reused archive: %d %v", n, err)
	}
}

func closeExportCache(t *testing.T, c *gitcache.Cache) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
