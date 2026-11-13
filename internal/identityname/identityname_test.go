package identityname

import (
	"strings"
	"testing"
)

func TestPerStepNameDeterministic(t *testing.T) {
	a := PerStepName("codeberg", "exampleorg", "exampleorg-port", ".oberth/release.yaml", "release-publish-images")
	b := PerStepName("codeberg", "exampleorg", "exampleorg-port", ".oberth/release.yaml", "release-publish-images")
	if a != b {
		t.Fatalf("not deterministic: %q != %q", a, b)
	}
}

func TestPerStepNameHasCorrectPrefix(t *testing.T) {
	name := PerStepName("codeberg", "exampleorg", "exampleorg-port", ".oberth/release.yaml", "release-publish-images")
	if !strings.HasPrefix(name, PerStepPrefix) {
		t.Fatalf("%q does not start with %q", name, PerStepPrefix)
	}
}

func TestPerStepNameFitsLabel(t *testing.T) {
	name := PerStepName("very-long-upstream", "very-long-organization-name", "very-long-repository-name-that-goes-on", ".oberth/release.yaml", "very-long-step-template-name")
	if len(name) > MaxNameLength {
		t.Fatalf("%q exceeds %d characters (%d)", name, MaxNameLength, len(name))
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
			t.Fatalf("%q contains non-DNS-1123 character %q", name, c)
		}
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		t.Fatalf("%q starts or ends with a hyphen", name)
	}
}

func TestPerStepNameFromQualified(t *testing.T) {
	direct := PerStepName("codeberg", "exampleorg", "exampleorg-port", ".oberth/release.yaml", "release-publish-images")
	fromQualified := PerStepNameFromQualified("codeberg/exampleorg/exampleorg-port", ".oberth/release.yaml", "release-publish-images")
	if direct != fromQualified {
		t.Fatalf("PerStepNameFromQualified %q != PerStepName %q", fromQualified, direct)
	}
}

func TestPerStepNameFromQualifiedBadFormat(t *testing.T) {
	if name := PerStepNameFromQualified("bare-repo", ".oberth/release.yaml", "step"); name != "" {
		t.Fatalf("expected empty for non-qualified repo, got %q", name)
	}
}

func TestHasPerStepPrefix(t *testing.T) {
	if !HasPerStepPrefix("oberth-step-foo-abc123456789") {
		t.Fatal("expected true for per-step name")
	}
	if HasPerStepPrefix("oberth-argo-foo-abc123456789") {
		t.Fatal("expected false for per-repo name")
	}
}
