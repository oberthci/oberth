package gitcache_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestFailedRefreshWaitsForBound(t *testing.T) {
	for _, failure := range []string{"unreachable", "deadline"} {
		t.Run(failure, func(t *testing.T) { checkFailedRefreshBound(t, failure) })
	}
}

func checkFailedRefreshBound(t *testing.T, failure string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	now := time.Unix(2000000000, 0)
	cfg.Now = func() time.Time { return now }
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "next", []byte("next"))
	next := f.CommitFile(t, "next")
	f.Git(t, f.Work, "push", f.Bare, "main")
	restore := failRefreshForge(t, f, failure)
	now = now.Add(time.Minute)
	notice := refreshFailureReads(t, c, f)
	if got := refreshAttempts(t, f); got != 1 {
		t.Fatalf("forge attempts within one bound = %d, want 1", got)
	}
	if notice != "Forge did not answer; serving cache aged 1m0s.\n" {
		t.Fatal("failure renewed cache age or lost the notice", notice)
	}
	if _, err := c.RemoteRef(t.Context(), gittest.Repository, "refs/heads/main"); err == nil {
		t.Fatal("forge-dependent read accepted the cached outage")
	}
	restore()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = reviewNew(t, cfg)
	now = now.Add(gitcache.RefreshInterval - time.Nanosecond)
	refreshFailureReads(t, c, f)
	if got := refreshAttempts(t, f); got != 1 {
		t.Fatal("restart or cached reads retried before the bound", got)
	}
	now = now.Add(time.Nanosecond)
	assertAdvertisement(t, c, next)
	var recovered bytes.Buffer
	if err := c.RefreshRead(t.Context(), gittest.Repository, &recovered); err != nil || recovered.Len() != 0 {
		t.Fatal("successful refresh kept the outage", err, recovered.String())
	}
	if got := refreshAttempts(t, f); got != 2 {
		t.Fatal("bound did not permit exactly one recovery attempt", got)
	}
}

func failRefreshForge(t *testing.T, f *gittest.Fixture, failure string) func() {
	t.Helper()
	if failure == "deadline" {
		reviewFault(t, f, map[string]any{"args": []string{"fetch", "--progress", f.Bare}, "stop": true})
		return func() { reviewDisarm(t, f) }
	}
	if err := os.Rename(f.Bare, f.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Rename(f.Bare+".offline", f.Bare); err != nil {
			t.Fatal(err)
		}
	}
}

func refreshFailureReads(t *testing.T, c *gitcache.Cache, f *gittest.Fixture) string {
	t.Helper()
	var notice bytes.Buffer
	for range 3 {
		notice.Reset()
		if err := c.RefreshRead(t.Context(), gittest.Repository, &notice); err != nil {
			t.Fatal(err)
		}
		assertAdvertisement(t, c, f.Commit)
	}
	return notice.String()
}

func refreshAttempts(t *testing.T, f *gittest.Fixture) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.Root, "commands.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' }) {
		var command struct{ Argv []string }
		if err := json.Unmarshal([]byte(line), &command); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(command.Argv, "fetch") && slices.Contains(command.Argv, f.Bare) {
			count++
		}
	}
	return count
}

func TestFailedRefreshKeepsForgeRefusal(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Now = func() time.Time { return time.Unix(2000000000, 0) }
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.Bare, f.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	for _, fetch := range []func() error{
		func() error { return c.ForceFetch(t.Context(), gittest.Repository) },
		func() error { return c.Fetch(t.Context(), gittest.Repository) },
	} {
		var outage *gitport.UnavailableError
		if err := fetch(); !errors.As(err, &outage) || outage.Op != "forge" {
			t.Fatal("refresh failure became success or a local error", err)
		}
	}
}
