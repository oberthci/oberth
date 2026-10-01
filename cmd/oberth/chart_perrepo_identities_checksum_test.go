package main

import (
	"os/exec"
	"strings"
	"testing"
)

const testImageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// TestChartPerRepoIdentitiesChecksumIsPresentAndStable proves that the
// checksum/per-repo-identities annotation is always emitted (unconditionally)
// and is deterministic for fixed identity lists.
func TestChartPerRepoIdentitiesChecksumIsPresentAndStable(t *testing.T) {
	t.Parallel()
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"--set", "argo.perRepoIdentities[0]=oberth-argo-codeberg-oberthci-oberth-abc123",
		"--set", "argo.perRepoCIIdentities[0]=oberth-argo-ci-codeberg-oberthci-oberth-def456",
	).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	output := string(rendered)
	if !strings.Contains(output, "checksum/per-repo-identities:") {
		t.Fatal("rendered chart is missing checksum/per-repo-identities annotation")
	}

	// Render again with the same values and verify the checksum is identical.
	rendered2, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"--set", "argo.perRepoIdentities[0]=oberth-argo-codeberg-oberthci-oberth-abc123",
		"--set", "argo.perRepoCIIdentities[0]=oberth-argo-ci-codeberg-oberthci-oberth-def456",
	).Output()
	if err != nil {
		t.Fatalf("render chart (second pass): %v", err)
	}
	checksum1 := extractAnnotation(t, string(rendered), "checksum/per-repo-identities:")
	checksum2 := extractAnnotation(t, string(rendered2), "checksum/per-repo-identities:")
	if checksum1 != checksum2 {
		t.Fatalf("checksum is not stable: %q != %q", checksum1, checksum2)
	}
}

// TestChartPerRepoIdentitiesChecksumChangesOnReleaseListChange proves that
// adding or removing a name from the release per-repo identity list changes
// the checksum, which triggers a pod restart.
func TestChartPerRepoIdentitiesChecksumChangesOnReleaseListChange(t *testing.T) {
	t.Parallel()
	base, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"--set", "argo.perRepoIdentities[0]=oberth-argo-codeberg-oberthci-oberth-abc123",
	).Output()
	if err != nil {
		t.Fatalf("render base chart: %v", err)
	}

	added, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"--set", "argo.perRepoIdentities[0]=oberth-argo-codeberg-oberthci-oberth-abc123",
		"--set", "argo.perRepoIdentities[1]=oberth-argo-codeberg-skipops-other-xyz789",
	).Output()
	if err != nil {
		t.Fatalf("render chart with added identity: %v", err)
	}

	baseChecksum := extractAnnotation(t, string(base), "checksum/per-repo-identities:")
	addedChecksum := extractAnnotation(t, string(added), "checksum/per-repo-identities:")
	if baseChecksum == addedChecksum {
		t.Fatal("checksum did not change when a release identity was added")
	}
}

// TestChartPerRepoIdentitiesChecksumChangesOnCIListChange proves that adding
// or removing a name from the CI per-repo identity list ALSO changes the
// checksum. A CI-tier-only grant change must restart the server too.
func TestChartPerRepoIdentitiesChecksumChangesOnCIListChange(t *testing.T) {
	t.Parallel()
	base, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"--set", "argo.perRepoCIIdentities[0]=oberth-argo-ci-codeberg-oberthci-oberth-abc123",
	).Output()
	if err != nil {
		t.Fatalf("render base chart: %v", err)
	}

	added, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"--set", "argo.perRepoCIIdentities[0]=oberth-argo-ci-codeberg-oberthci-oberth-abc123",
		"--set", "argo.perRepoCIIdentities[1]=oberth-argo-ci-codeberg-skipops-other-xyz789",
	).Output()
	if err != nil {
		t.Fatalf("render chart with added CI identity: %v", err)
	}

	baseChecksum := extractAnnotation(t, string(base), "checksum/per-repo-identities:")
	addedChecksum := extractAnnotation(t, string(added), "checksum/per-repo-identities:")
	if baseChecksum == addedChecksum {
		t.Fatal("checksum did not change when a CI identity was added")
	}
}

// TestChartPerRepoIdentitiesChecksumRendersCleanWithoutLists proves that the
// chart renders cleanly when both identity lists are absent — the legacy
// --reuse-values case where a prior release never set them.
func TestChartPerRepoIdentitiesChecksumRendersCleanWithoutLists(t *testing.T) {
	t.Parallel()
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
	).Output()
	if err != nil {
		t.Fatalf("render chart without identity lists: %v", err)
	}
	output := string(rendered)
	if !strings.Contains(output, "checksum/per-repo-identities:") {
		t.Fatal("checksum/per-repo-identities annotation must be present even when both lists are empty")
	}
}

// extractAnnotation returns the full line containing the given annotation key
// prefix from the rendered YAML, trimmed. Fails the test if not found.
func extractAnnotation(t *testing.T, rendered, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return trimmed
		}
	}
	t.Fatalf("annotation %q not found in rendered output", prefix)
	return ""
}
