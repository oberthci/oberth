package installer

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/identityname"
)

func TestStepPoliciesIsolateWorkflowFiles(t *testing.T) {
	grants := []grantWithStep{
		{upstream: "upstream", org: "org", repo: "repo", workflow: ".oberth/a.yaml", step: "*", secret: "release/a"},
		{upstream: "upstream", org: "org", repo: "repo", workflow: ".oberth/a.yaml", step: "publish", secret: "release/signing"},
		{upstream: "upstream", org: "org", repo: "repo", workflow: ".oberth/b.yaml", step: "*", secret: "release/b"},
	}
	identities := DerivePerStepIdentities(nil, grants)
	if len(identities) != 3 {
		t.Fatal(identities)
	}
	for _, id := range identities {
		for _, p := range id.GrantPaths {
			if id.Workflow == ".oberth/a.yaml" && p == "release/b" || id.Workflow == ".oberth/b.yaml" && p != "release/b" {
				t.Fatalf("cross-file policy inheritance: %+v", id)
			}
		}
	}
	a := PerStepName("upstream", "org", "repo", ".oberth/a.yaml", "publish")
	b := PerStepName("upstream", "org", "repo", ".oberth/b.yaml", "publish")
	if a == b {
		t.Fatal("workflow files share a login identity", a)
	}
}

func TestPerStepNameDeterministic(t *testing.T) {
	a := PerStepName("codeberg", "exampleorg", "exampleorg-port", ".oberth/release.yaml", "release-publish-images")
	b := PerStepName("codeberg", "exampleorg", "exampleorg-port", ".oberth/release.yaml", "release-publish-images")
	if a != b {
		t.Fatalf("PerStepName is not deterministic: %q != %q", a, b)
	}
}

func TestPerStepNameHasCorrectPrefix(t *testing.T) {
	name := PerStepName("codeberg", "exampleorg", "exampleorg-port", ".oberth/release.yaml", "release-publish-images")
	if !strings.HasPrefix(name, identityname.PerStepPrefix) {
		t.Fatalf("PerStepName %q does not start with %q", name, identityname.PerStepPrefix)
	}
}

func TestPerStepNameFitsLabel(t *testing.T) {
	// Even with long inputs, the name must fit in a Kubernetes label value (63 chars).
	name := PerStepName("very-long-upstream", "very-long-organization-name", "very-long-repository-name-that-goes-on", ".oberth/release.yaml", "very-long-step-template-name")
	if len(name) > maxPerRepoNameLength {
		t.Fatalf("PerStepName %q exceeds %d characters (%d)", name, maxPerRepoNameLength, len(name))
	}
}

func TestPerStepNameDistinctFromPerRepo(t *testing.T) {
	perRepo := PerRepoName("codeberg", "exampleorg", "exampleorg-port")
	perStep := PerStepName("codeberg", "exampleorg", "exampleorg-port", ".oberth/release.yaml", "release-publish-images")
	if perRepo == perStep {
		t.Fatal("per-step and per-repo names must differ")
	}
}

func TestPerStepNameDistinctAcrossSteps(t *testing.T) {
	a := PerStepName("codeberg", "exampleorg", "exampleorg-port", ".oberth/release.yaml", "release-publish-images")
	b := PerStepName("codeberg", "exampleorg", "exampleorg-port", ".oberth/release.yaml", "release-publish-r2")
	if a == b {
		t.Fatalf("different steps produce the same name: %q", a)
	}
}

func TestPerStepNameDNS1123Safe(t *testing.T) {
	name := PerStepName("codeberg", "example.org", "my_repo", ".oberth/release.yaml", "my-step")
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
	policy := PerStepPolicy("oberth", "exampleorg", "exampleorg-port",
		[]string{"release/gar-image-key", "upstream/exampleorg/exampleorg-port/test-secret"})

	if !strings.Contains(policy, `path "oberth/data/release/gar-image-key"`) {
		t.Fatal("policy must contain the first grant path")
	}
	if !strings.Contains(policy, `path "oberth/data/upstream/exampleorg/exampleorg-port/test-secret"`) {
		t.Fatal("policy must contain the second grant path")
	}
}

func TestPerStepPolicyContainsUpstreamWildcard(t *testing.T) {
	policy := PerStepPolicy("oberth", "exampleorg", "exampleorg-port",
		[]string{"release/gar-image-key"})

	if !strings.Contains(policy, `path "oberth/data/upstream/exampleorg/exampleorg-port/*"`) {
		t.Fatal("policy must contain the upstream org/repo wildcard")
	}
}

func TestPerStepPolicyContainsSelfRevoke(t *testing.T) {
	policy := PerStepPolicy("oberth", "exampleorg", "exampleorg-port",
		[]string{"release/gar-image-key"})

	if !strings.Contains(policy, `path "auth/token/revoke-self"`) {
		t.Fatal("policy must contain self-revoke path")
	}
}

func TestPerStepPolicyDeniesServerIdentities(t *testing.T) {
	policy := PerStepPolicy("oberth", "exampleorg", "exampleorg-port",
		[]string{"release/gar-image-key"})

	if !strings.Contains(policy, `path "oberth/data/identities/*"`) {
		t.Fatal("policy must deny server identities")
	}
}

func TestPerStepPolicyDeduplicatesPaths(t *testing.T) {
	policy := PerStepPolicy("oberth", "exampleorg", "exampleorg-port",
		[]string{"release/gar-image-key", "release/gar-image-key"})

	count := strings.Count(policy, "gar-image-key")
	if count != 1 {
		t.Fatalf("expected deduplicated paths, got %d occurrences", count)
	}
}

func TestPerStepPolicyNarrowerThanPerRepo(t *testing.T) {
	// Per-repo policy includes all grant paths.
	perRepoPolicy := PerRepoPolicy("oberth", "exampleorg", "exampleorg-port",
		[]string{"release/gar-image-key", "release/cosign-secret", "release/r2-upload-token"})

	// Per-step policy includes only the step's paths.
	perStepPolicy := PerStepPolicy("oberth", "exampleorg", "exampleorg-port",
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
		{workflow: ".oberth/release.yaml", upstream: "codeberg", org: "exampleorg", repo: "exampleorg-port", step: "*", secret: "oberth/data/release/gar-image-key"},
	}
	result := DerivePerStepIdentities(nil, grants)
	if len(result) != 1 || result[0].Step != "*" || result[0].Workflow != ".oberth/release.yaml" {
		t.Fatalf("wildcard-only grants must produce one workflow-scoped identity, got %d", len(result))
	}
}

func TestDerivePerStepIdentitiesNamedGrant(t *testing.T) {
	grants := []grantWithStep{
		{workflow: ".oberth/release.yaml", upstream: "codeberg", org: "exampleorg", repo: "exampleorg-port", step: "release-publish-images", secret: "oberth/data/release/gar-image-key"},
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
		{workflow: ".oberth/release.yaml", upstream: "codeberg", org: "exampleorg", repo: "exampleorg-port", step: "*", secret: "oberth/data/release/r2-token"},
		{workflow: ".oberth/release.yaml", upstream: "codeberg", org: "exampleorg", repo: "exampleorg-port", step: "release-publish-images", secret: "oberth/data/release/gar-image-key"},
	}
	result := DerivePerStepIdentities(nil, grants)
	if len(result) != 2 {
		t.Fatalf("expected 1 per-step identity, got %d", len(result))
	}
	// The per-step identity should include both the named grant and the
	// inherited wildcard-granted path.
	if len(result[1].GrantPaths) != 2 {
		t.Fatalf("expected 2 grant paths (named + inherited wildcard), got %d: %v",
			len(result[1].GrantPaths), result[1].GrantPaths)
	}
}

func TestDerivePerStepIdentitiesMultipleSteps(t *testing.T) {
	grants := []grantWithStep{
		{workflow: ".oberth/release.yaml", upstream: "codeberg", org: "exampleorg", repo: "exampleorg-port", step: "release-publish-images", secret: "oberth/data/release/gar-image-key"},
		{workflow: ".oberth/release.yaml", upstream: "codeberg", org: "exampleorg", repo: "exampleorg-port", step: "release-publish-r2", secret: "oberth/data/release/r2-token"},
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
		{workflow: ".oberth/release.yaml", upstream: "codeberg", org: "exampleorg", repo: "repo-a", step: "release-publish", secret: "oberth/data/release/key-a"},
		{workflow: ".oberth/release.yaml", upstream: "codeberg", org: "exampleorg", repo: "repo-b", step: "release-publish", secret: "oberth/data/release/key-b"},
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
		{Workflow: ".oberth/release.yaml", Upstream: "codeberg", Org: "exampleorg", Repo: "zzz-repo", Step: "step-a"},
		{Workflow: ".oberth/release.yaml", Upstream: "codeberg", Org: "exampleorg", Repo: "aaa-repo", Step: "step-b"},
	}
	names := PerStepIdentityNames(identities)
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}
	if names[0] > names[1] {
		t.Fatalf("names not sorted: %v", names)
	}
}
