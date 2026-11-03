package gitcache

import (
	"errors"
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
	runGit(t, r.work, "tag", "-a", "park-object", "-m", "park tag", source)
	tag := runGit(t, r.work, "rev-parse", "refs/tags/park-object")
	runGit(t, r.work, "push", ready.Path, "refs/tags/park-object")
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
