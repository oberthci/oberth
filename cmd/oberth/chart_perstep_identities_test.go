package main

import (
	"os/exec"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Per-step identity chart tests (issue #623)
// ---------------------------------------------------------------------------

// TestChartPerStepIdentitiesLegacyReusedValues simulates `helm upgrade
// --reuse-values` on a pre-#623 release: argo.perStepIdentities is absent
// from the values entirely. The chart must render cleanly and produce zero
// per-step ServiceAccounts. This catches the helm --reuse-values gap where
// a newly added value is never consulted when upgrading from a release that
// predates it.
func TestChartPerStepIdentitiesLegacyReusedValues(t *testing.T) {
	t.Parallel()

	// Render without setting perStepIdentities at all — the chart's default
	// empty list applies, same as --reuse-values from a pre-#623 release.
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
	).Output()
	if err != nil {
		t.Fatalf("render chart without perStepIdentities: %v", err)
	}

	output := string(rendered)

	// The render must succeed (it did, since err is nil). Now verify zero
	// per-step SAs were produced.
	if strings.Contains(output, "oberth.ci/identity-type: per-step") {
		t.Fatal("chart rendered per-step ServiceAccounts when perStepIdentities was absent; " +
			"a pre-#623 --reuse-values upgrade must produce zero per-step identities")
	}
}

// TestChartPerStepIdentitiesTwoNamesRenderExactSAs sets two per-step identity
// names and asserts that exactly those two ServiceAccounts are rendered with
// automountServiceAccountToken: false and the per-step identity-type label.
func TestChartPerStepIdentitiesTwoNamesRenderExactSAs(t *testing.T) {
	t.Parallel()

	const (
		sa1 = "oberth-argo-step-publish-acme-abc123"
		sa2 = "oberth-argo-step-deploy-acme-def456"
	)

	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"--set", "argo.perStepIdentities[0]="+sa1,
		"--set", "argo.perStepIdentities[1]="+sa2,
	).Output()
	if err != nil {
		t.Fatalf("render chart with two perStepIdentities: %v", err)
	}

	output := string(rendered)

	// Split the rendered output into YAML documents.
	documents := strings.Split(output, "---")

	// Find ServiceAccounts with per-step identity type.
	var perStepDocs []string
	for _, doc := range documents {
		if strings.Contains(doc, "oberth.ci/identity-type: per-step") &&
			strings.Contains(doc, "kind: ServiceAccount") {
			perStepDocs = append(perStepDocs, doc)
		}
	}

	if len(perStepDocs) != 2 {
		t.Fatalf("expected exactly 2 per-step ServiceAccount documents, got %d", len(perStepDocs))
	}

	// Verify both names appear.
	foundSA1, foundSA2 := false, false
	for _, doc := range perStepDocs {
		if strings.Contains(doc, "name: "+sa1) {
			foundSA1 = true
		}
		if strings.Contains(doc, "name: "+sa2) {
			foundSA2 = true
		}
		// Each must have automountServiceAccountToken: false.
		if !strings.Contains(doc, "automountServiceAccountToken: false") {
			t.Fatalf("per-step SA missing automountServiceAccountToken: false:\n%s", doc)
		}
	}

	if !foundSA1 {
		t.Fatalf("per-step SA %q not found in rendered output", sa1)
	}
	if !foundSA2 {
		t.Fatalf("per-step SA %q not found in rendered output", sa2)
	}
}

// TestChartPerStepIdentitiesChecksumStabilityWithPerStepList verifies that
// adding per-step identities does not break the existing per-repo-identities
// checksum: the two identity mechanisms are independent.
func TestChartPerStepIdentitiesChecksumStabilityWithPerStepList(t *testing.T) {
	t.Parallel()

	// Render with only per-step identities, no per-repo.
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"--set", "argo.perStepIdentities[0]=oberth-argo-step-test-abc",
	).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}

	// The per-repo checksum must still be present (it does not depend on
	// per-step identities).
	if !strings.Contains(string(rendered), "checksum/per-repo-identities:") {
		t.Fatal("per-repo-identities checksum must be present even with per-step identities set")
	}
}
