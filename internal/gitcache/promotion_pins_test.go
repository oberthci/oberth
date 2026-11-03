package gitcache

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromotionPinsLeaveTrackingAndAreReleasedAfterVerifiedPush(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ctx := t.Context()
	ready, err := c.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	feature := r.featureCommit(t, "feature/pin", "feature\n")
	runGit(t, r.work, "push", ready.Path, feature+":refs/heads/feature/pin")
	candidate, err := c.PreparePromotion(ctx, "example", feature, "main", filepath.Join(t.TempDir(), "merge"))
	if err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, ready.Path, "for-each-ref", "--format=%(refname)", upstreamRefPrefix+"promotions/"); got != "" {
		t.Errorf("promotion pins pollute tracking: %s", got)
	}
	if err := c.PushPromotion(ctx, "example", "main", candidate.MergedSHA); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, ready.Path, "for-each-ref", "--format=%(refname)", "refs/oberth/"); strings.Contains(got, "promotions/") {
		t.Errorf("verified promotion retains pin: %s", got)
	}
}
func TestLegacyPromotionPinsMigrateWithoutLosingObjects(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	sha := r.featureCommit(t, "feature/pending", "pending\n")
	runGit(t, r.work, "push", ready.Path, sha+":refs/heads/feature/pending")
	legacy := upstreamRefPrefix + "promotions/main/" + sha
	runGit(t, ready.Path, "update-ref", legacy, sha)
	if _, err = c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, ready.Path, "for-each-ref", "--format=%(refname)", upstreamRefPrefix+"promotions/"); got != "" {
		t.Fatalf("legacy pins remain in tracking: %s", got)
	}
	if got := runGit(t, ready.Path, "for-each-ref", "--format=%(objectname)", "refs/oberth/promotions/"); !strings.Contains(got, sha) {
		t.Fatal("migration lost pending promotion pin")
	}
}

func TestPublicationReadbackReleasesPromotionPin(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	ref := promotionRefPrefix + "main/" + r.initialSHA
	runGit(t, ready.Path, "update-ref", ref, r.initialSHA)
	sha, exists, err := c.RemoteRef(t.Context(), "example", "refs/heads/main")
	if err != nil || !exists || sha != r.initialSHA {
		t.Fatalf("readback: %s %t %v", sha, exists, err)
	}
	if got := runGit(t, ready.Path, "for-each-ref", "--format=%(refname)", promotionRefPrefix); got != "" {
		t.Fatalf("confirmed delivery kept pin: %s", got)
	}
}
func TestPushPromotionWithoutPinSucceeds(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if err := c.PushPromotion(t.Context(), "example", "main", r.initialSHA); err != nil {
		t.Fatal(err)
	}
}
func TestRefreshSweepsAlreadyDeliveredLegacyPromotionPin(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	sha := r.commit(t, "delivered", "delivered\\n")
	runGit(t, ready.Path, "fetch", "upstream", sha)
	runGit(t, ready.Path, "update-ref", upstreamRefPrefix+"promotions/main/"+sha, sha)
	if _, err = c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, ready.Path, "for-each-ref", "--format=%(refname)", "refs/oberth/"); strings.Contains(got, "promotions/") {
		t.Fatalf("delivered legacy pin survives refresh: %s", got)
	}
}

func TestReleaseAbandonedPromotionPinIsLocalExactAndIdempotent(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	abandoned := r.featureCommit(t, "feature/abandoned", "abandoned\n")
	runGit(t, r.work, "push", ready.Path, abandoned+":refs/heads/feature/abandoned")
	ref := promotionRefPrefix + "main/" + abandoned
	other := promotionRefPrefix + "release/" + abandoned
	runGit(t, ready.Path, "update-ref", ref, abandoned)
	runGit(t, ready.Path, "update-ref", other, abandoned)
	releaser, ok := any(c).(interface {
		ReleasePromotionPin(context.Context, string, string, string) error
	})
	if !ok {
		t.Fatal("cache cannot release an abandoned promotion pin")
	}
	for range 2 {
		if err := releaser.ReleasePromotionPin(t.Context(), "example", "main", abandoned); err != nil {
			t.Fatal(err)
		}
	}
	if got := runGit(t, ready.Path, "for-each-ref", "--format=%(refname)", ref); got != "" {
		t.Fatalf("abandoned pin retained: %s", got)
	}
	if got := runGit(t, ready.Path, "rev-parse", other); got != abandoned {
		t.Fatal("cleanup removed another promotion pin")
	}
	if got := runGit(t, r.upstream, "rev-parse", "refs/heads/main"); got != r.initialSHA {
		t.Fatal("cleanup changed the upstream")
	}
	if err := releaser.ReleasePromotionPin(t.Context(), "example", "../main", abandoned); err == nil {
		t.Fatal("invalid branch accepted")
	}
	if err := releaser.ReleasePromotionPin(t.Context(), "example", "main", "invalid"); err == nil {
		t.Fatal("invalid SHA accepted")
	}
}
