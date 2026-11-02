package gitcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSHA256ValidationMatchesSharedObjectIDs(t *testing.T) {
	if err := ValidateSHA(strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}
func TestReadDefaultBlobHonorsArchiveGate(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	blocked := errors.New("archived")
	c.repositoryGate = func(context.Context, string) error { return blocked }
	if _, _, _, err := c.ReadDefaultBlob(t.Context(), "example", "file.txt", 100); !errors.Is(err, blocked) {
		t.Fatalf("archive gate bypassed: %v", err)
	}
}
func TestCaptureSeparatesDiagnosticsAndRejectsTruncation(t *testing.T) {
	c := newTestCache(t, "unused")
	script := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nif [ \"$1\" = large ]; then head -c 70000 /dev/zero | tr '\\000' a; else echo result; echo diagnostic >&2; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.gitBinary = script
	got, err := c.capture(t.Context(), "", "small")
	if err != nil || got != "result" {
		t.Errorf("stdout polluted: %q %v", got, err)
	}
	if _, err := c.capture(t.Context(), "", "large"); err == nil {
		t.Error("capture silently truncated stdout")
	}
}

func TestProductionCacheRefusesLocalUpstreams(t *testing.T) {
	r := newTestRepository(t)
	c, err := New(Config{Root: t.TempDir(), Upstream: func(string) (string, error) { return r.upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Ensure(t.Context(), "example"); err == nil {
		t.Fatal("production accepted a local filesystem upstream")
	}
}

func TestProductionCacheRefusesPreviouslyCachedLocalUpstream(t *testing.T) {
	r := newTestRepository(t)
	allowed := newTestCache(t, r.upstream)
	if _, err := allowed.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	production, err := New(Config{Root: allowed.root, Upstream: func(string) (string, error) { return r.upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, ensure := range []func(context.Context, string) (Repository, error){production.Ensure, production.EnsureForFetch} {
		if _, err = ensure(t.Context(), "example"); !errors.Is(err, ErrUpstreamRefused) {
			t.Fatalf("cached local upstream bypassed production refusal: %v", err)
		}
	}
}
