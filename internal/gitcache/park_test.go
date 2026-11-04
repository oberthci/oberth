package gitcache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParkSourceRequiresReachablePublicBranch(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	source := r.featureCommit(t, "review", "review\n")
	runGit(t, r.work, "push", ready.Path, source+":refs/heads/park/review")
	for _, tc := range []struct{ sha, prefix, want string }{
		{source, "refs/heads/park/", "park/review"},
		{r.initialSHA, "refs/heads/park/", "park/review"},
		{strings.Repeat("f", 40), "refs/heads/park/", ""},
		{source, "refs/heads/review/", ""},
	} {
		ref, err := c.ParkSource(t.Context(), "example", tc.sha, tc.prefix)
		if err != nil {
			t.Fatal(err)
		}
		if ref != tc.want {
			t.Fatalf("ParkSource(%s,%s)=%q, want %q", tc.sha, tc.prefix, ref, tc.want)
		}
	}
	runGit(t, ready.Path, "update-ref", "-d", "refs/heads/park/review")
	for _, ref := range []string{"refs/heads/parked/review", "refs/tags/park/review", "refs/oberth/promotions/park/review"} {
		runGit(t, ready.Path, "update-ref", ref, source)
	}
	if ref, err := c.ParkSource(t.Context(), "example", source, "refs/heads/park/"); err != nil || ref != "" {
		t.Fatalf("non-park provenance=%q %v", ref, err)
	}
	// Object-type checks must not depend on the cache's receive admission.
	rejectHook := filepath.Join(ready.Path, "hooks", "reject-receive")
	if err := os.WriteFile(rejectHook, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rejectHook, filepath.Join(ready.Path, "hooks", "pre-receive")); err != nil {
		t.Fatal(err)
	}
	runGit(t, ready.Path, "config", "core.hooksPath", filepath.Join(ready.Path, "hooks"))
	tag := runGitStdin(t, ready.Path, "object "+source+"\ntype commit\ntag park-object\ntagger Fixture <fixture@example.invalid> 0 +0000\n\npark tag\n", "mktag")
	runGit(t, ready.Path, "update-ref", "refs/tags/park-object", tag)
	if _, err := c.ParkSource(t.Context(), "example", tag, "refs/heads/park/"); err == nil {
		t.Fatal("tag object accepted as parked commit")
	}
	for _, prefix := range []string{"refs/heads/", "refs/tags/park/", "refs/heads/park", "refs/heads/*/"} {
		if _, err := c.ParkSource(t.Context(), "example", source, prefix); err == nil {
			t.Errorf("accepted prefix %q", prefix)
		}
	}
}

func TestUncachedRepositoryHasNoParkProvenance(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	if _, _, _, err := c.ReadDefaultBlob(t.Context(), "example", ".oberth/promotion.yaml", 16384); !errors.Is(err, ErrPolicyRepositoryNotCached) {
		t.Fatalf("uncached policy error=%v", err)
	}
}
