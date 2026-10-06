package installer

import (
	"strings"
	"testing"
)

func TestPerStepNameDeterministic(t *testing.T) {
	a := PerStepName("codeberg", "cloudtaser", "cloudtaser-port", "release-publish-images")
	b := PerStepName("codeberg", "cloudtaser", "cloudtaser-port", "release-publish-images")
	if a != b {
		t.Fatalf("PerStepName is not deterministic: %q != %q", a, b)
	}
}

func TestPerStepNameHasCorrectPrefix(t *testing.T) {
	name := PerStepName("codeberg", "cloudtaser", "cloudtaser-port", "release-publish-images")
	if !strings.HasPrefix(name, perStepNamePrefix) {
		t.Fatalf("PerStepName %q does not start with %q", name, perStepNamePrefix)
	}
}

func TestPerStepNameFitsLabel(t *testing.T) {
	// Even with long inputs, the name must fit in a Kubernetes label value (63 chars).
	name := PerStepName("very-long-upstream", "very-long-organization-name", "very-long-repository-name-that-goes-on", "very-long-step-template-name")
	if len(name) > maxPerRepoNameLength {
		t.Fatalf("PerStepName %q exceeds %d characters (%d)", name, maxPerRepoNameLength, len(name))
	}
}

func TestPerStepNameDistinctFromPerRepo(t *testing.T) {
	perRepo := PerRepoName("codeberg", "cloudtaser", "cloudtaser-port")
	perStep := PerStepName("codeberg", "cloudtaser", "cloudtaser-port", "release-publish-images")
	if perRepo == perStep {
		t.Fatal("per-step and per-repo names must differ")
	}
}

func TestPerStepNameDistinctAcrossSteps(t *testing.T) {
	a := PerStepName("codeberg", "cloudtaser", "cloudtaser-port", "release-publish-images")
	b := PerStepName("codeberg", "cloudtaser", "cloudtaser-port", "release-publish-r2")
	if a == b {
		t.Fatalf("different steps produce the same name: %q", a)
	}
}

func TestPerStepNameDNS1123Safe(t *testing.T) {
	name := PerStepName("codeberg", "cloud.taser", "my_repo", "my-step")
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			t.Fatalf("PerStepName %q contains non-DNS-1123 character %q", name, c)
		}
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		t.Fatalf("PerStepName %q starts or ends with a hyphen", name)
	}
}

func TestPerStepPolicyContainsGrantPaths(t *testing.T) {
	policy := PerStepPolicy("oberth", "cloudtaser", "cloudtaser-port",
		[]string{"release/gar-image-key", "upstream/cloudtaser/cloudtaser-port/test-secret"})

	if !strings.Contains(policy, `path "oberth/data/release/gar-image-key"`) {
		t.Fatal("policy must contain the first grant path")
	}
	if !strings.Contains(policy, `path "oberth/data/upstream/cloudtaser/cloudtaser-port/test-secret"`) {
		t.Fatal("policy must contain the second grant path")
	}
}

func TestPerStepPolicyContainsUpstreamWildcard(t *testing.T) {
	policy := PerStepPolicy("oberth", "cloudtaser", "cloudtaser-port",
		[]string{"release/gar-image-key"})

	if !strings.Contains(policy, `path "oberth/data/upstream/cloudtaser/cloudtaser-port/*"`) {
		t.Fatal("policy must contain the upstream org/repo wildcard")
	}
}

func TestPerStepPolicyContainsSelfRevoke(t *testing.T) {
	policy := PerStepPolicy("oberth", "cloudtaser", "cloudtaser-port",
		[]string{"release/gar-image-key"})

	if !strings.Contains(policy, `path "auth/token/revoke-self"`) {
		t.Fatal("policy must contain self-revoke path")
	}
}

func TestPerStepPolicyDeniesServerIdentities(t *testing.T) {
	policy := PerStepPolicy("oberth", "cloudtaser", "cloudtaser-port",
		[]string{"release/gar-image-key"})

	if !strings.Contains(policy, `path "oberth/data/identities/*"`) {
		t.Fatal("policy must deny server identities")
	}
}

func TestPerStepPolicyDeduplicatesPaths(t *testing.T) {
	policy := PerStepPolicy("oberth", "cloudtaser", "cloudtaser-port",
		[]string{"release/gar-image-key", "release/gar-image-key"})

	count := strings.Count(policy, "gar-image-key")
	if count != 1 {
		t.Fatalf("expected deduplicated paths, got %d occurrences", count)
	}
}

func TestPerStepPolicyNarrowerThanPerRepo(t *testing.T) {
	// Per-repo policy includes all grant paths.
	perRepoPolicy := PerRepoPolicy("oberth", "cloudtaser", "cloudtaser-port",
		[]string{"release/gar-image-key", "release/cosign-secret", "release/r2-upload-token"})

	// Per-step policy includes only the step's paths.
	perStepPolicy := PerStepPolicy("oberth", "cloudtaser", "cloudtaser-port",
		[]string{"release/gar-image-key"})

	if !strings.Contains(perRepoPolicy, "cosign-secret") {
		t.Fatal("per-repo policy should contain cosign-secret")
	}
	if strings.Contains(perStepPolicy, "cosign-secret") {
		t.Fatal("per-step policy must NOT contain cosign-secret (scoped to different step)")
	}
}

func TestDerivePerStepIdentitiesWildcardOnly(t *testing.T) {
	grants := []grantWithStep{
		{upstream: "codeberg", org: "cloudtaser", repo: "cloudtaser-port", step: "*", secret: "oberth/data/release/gar-image-key"},
	}
	result := DerivePerStepIdentities(nil, grants)
	if len(result) != 0 {
		t.Fatalf("wildcard-only grants should produce zero per-step identities, got %d", len(result))
	}
}

func TestDerivePerStepIdentitiesNamedGrant(t *testing.T) {
	grants := []grantWithStep{
		{upstream: "codeberg", org: "cloudtaser", repo: "cloudtaser-port", step: "release-publish-images", secret: "oberth/data/release/gar-image-key"},
	}
	result := DerivePerStepIdentities(nil, grants)
	if len(result) != 1 {
		t.Fatalf("expected 1 per-step identity, got %d", len(result))
	}
	if result[0].Step != "release-publish-images" {
		t.Fatalf("expected step 'release-publish-images', got %q", result[0].Step)
	}
	if len(result[0].GrantPaths) != 1 || result[0].GrantPaths[0] != "oberth/data/release/gar-image-key" {
		t.Fatalf("unexpected grant paths: %v", result[0].GrantPaths)
	}
}

func TestDerivePerStepIdentitiesInheritsWildcard(t *testing.T) {
	grants := []grantWithStep{
		{upstream: "codeberg", org: "cloudtaser", repo: "cloudtaser-port", step: "*", secret: "oberth/data/release/r2-token"},
		{upstream: "codeberg", org: "cloudtaser", repo: "cloudtaser-port", step: "release-publish-images", secret: "oberth/data/release/gar-image-key"},
	}
	result := DerivePerStepIdentities(nil, grants)
	if len(result) != 1 {
		t.Fatalf("expected 1 per-step identity, got %d", len(result))
	}
	// The per-step identity should include both the named grant and the
	// inherited wildcard-granted path.
	if len(result[0].GrantPaths) != 2 {
		t.Fatalf("expected 2 grant paths (named + inherited wildcard), got %d: %v",
			len(result[0].GrantPaths), result[0].GrantPaths)
	}
}

func TestDerivePerStepIdentitiesMultipleSteps(t *testing.T) {
	grants := []grantWithStep{
		{upstream: "codeberg", org: "cloudtaser", repo: "cloudtaser-port", step: "release-publish-images", secret: "oberth/data/release/gar-image-key"},
		{upstream: "codeberg", org: "cloudtaser", repo: "cloudtaser-port", step: "release-publish-r2", secret: "oberth/data/release/r2-token"},
	}
	result := DerivePerStepIdentities(nil, grants)
	if len(result) != 2 {
		t.Fatalf("expected 2 per-step identities, got %d", len(result))
	}
	// Verify they have different steps.
	if result[0].Step == result[1].Step {
		t.Fatal("expected different steps for different per-step identities")
	}
}

func TestDerivePerStepIdentitiesCrossRepoIsolation(t *testing.T) {
	grants := []grantWithStep{
		{upstream: "codeberg", org: "cloudtaser", repo: "repo-a", step: "release-publish", secret: "oberth/data/release/key-a"},
		{upstream: "codeberg", org: "cloudtaser", repo: "repo-b", step: "release-publish", secret: "oberth/data/release/key-b"},
	}
	result := DerivePerStepIdentities(nil, grants)
	if len(result) != 2 {
		t.Fatalf("expected 2 per-step identities, got %d", len(result))
	}
	// Verify each identity only has its own repo's path.
	for _, id := range result {
		if id.Repo == "repo-a" && id.GrantPaths[0] != "oberth/data/release/key-a" {
			t.Fatalf("repo-a identity has wrong path: %v", id.GrantPaths)
		}
		if id.Repo == "repo-b" && id.GrantPaths[0] != "oberth/data/release/key-b" {
			t.Fatalf("repo-b identity has wrong path: %v", id.GrantPaths)
		}
	}
}

func TestPerStepIdentityNamesSorted(t *testing.T) {
	identities := []PerStepIdentity{
		{Upstream: "codeberg", Org: "cloudtaser", Repo: "zzz-repo", Step: "step-a"},
		{Upstream: "codeberg", Org: "cloudtaser", Repo: "aaa-repo", Step: "step-b"},
	}
	names := PerStepIdentityNames(identities)
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}
	if names[0] > names[1] {
		t.Fatalf("names not sorted: %v", names)
	}
}
