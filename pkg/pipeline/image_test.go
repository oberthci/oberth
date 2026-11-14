package pipeline

import (
	"strings"
	"testing"
)

func TestRunnerImageAllowlistUsesRegistryAndRepositoryBoundaries(t *testing.T) {
	for _, test := range []struct {
		image    string
		prefixes []string
		allowed  bool
	}{
		{"golang:5000/evil", nil, true},
		{"ghcr.io/org-evil/x", []string{"ghcr.io/org"}, false},
		{"ghcr.io/org/x", []string{"ghcr.io/org"}, true},
		{"ghcr.io/org/x", []string{"ghcr.io/org/"}, true},
		{"golang:1.26", nil, true},
		{"docker.io/library/golang:1.26", nil, true},
		{"index.docker.io/library/golang:1.26", []string{"docker.io/library/golang:"}, true},
		{"golang:1.26", []string{"docker.io/library/golang:"}, true},
		{"golang:1.25", []string{"golang:1.26"}, false},
		{"golang:1.26-alpine", []string{"golang:1.26"}, true},
		{"registry.example:5000/base/go:1", []string{"registry.example:5000/base/"}, true},
		{"registry.example:5001/base/go:1", []string{"registry.example:5000/base/"}, false},
		{"ghcr.io/org/x", []string{"ghcr.io/"}, true},
		{"aquasec/trivy:1", []string{"aquasec/"}, true},
		{"aquasec-evil/trivy:1", []string{"aquasec/"}, false},
	} {
		t.Run(test.image+strings.Join(test.prefixes, ","), func(t *testing.T) {
			image := test.image + "@sha256:" + strings.Repeat("a", 64)
			if err := ValidateRunnerImage(image); err != nil {
				t.Fatal(err)
			}
			if got := RunnerImageAllowed(image, test.prefixes); got != test.allowed {
				t.Fatalf("allowed=%v, want %v", got, test.allowed)
			}
		})
	}
}

func TestRunnerImageAllowedAgainstPrefixes(t *testing.T) {
	if !RunnerImageAllowed("golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil) {
		t.Fatal("empty allowlist must admit authored images")
	}
	if !RunnerImageAllowed("evil.example/golang:1.26@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil) {
		t.Fatal("empty allowlist refused an authored image")
	}
	prefixes := []string{"golang:", "registry.example/base/"}
	if !RunnerImageAllowed("registry.example/base/go:1.26@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", prefixes) {
		t.Fatal("configured prefix rejected")
	}
	if RunnerImageAllowed("registry.example/other:1.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", prefixes) {
		t.Fatal("non-matching image admitted")
	}
}

func TestValidateRunnerImagePrefixes(t *testing.T) {
	if err := ValidateRunnerImagePrefixes([]string{"golang:", "registry.example/base/"}); err != nil {
		t.Fatal(err)
	}
	for _, prefixes := range [][]string{
		{""},
		{" golang:"},
		{"golang:", "golang:"},
		{"gol ang:"},
	} {
		if err := ValidateRunnerImagePrefixes(prefixes); err == nil {
			t.Fatalf("prefixes %q admitted", prefixes)
		}
	}
}

func TestValidateRunnerImageAcceptsGoodImages(t *testing.T) {
	for _, image := range []string{
		// tag + digest (the recommended form)
		"golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"golang:1.26-trixie@sha256:7202554ed358a37f41c44e4910ae29bbc5ef0715f289ac637218adbb0ca132dd",
		"registry.example/base/go:1.26@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		// digest-only (no tag, but repository + digest)
		"golang@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"registry.example/base/go@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	} {
		if err := ValidateRunnerImage(image); err != nil {
			t.Errorf("ValidateRunnerImage(%q) = %v", image, err)
		}
	}
}

func TestValidateRunnerImageRejectsBadImages(t *testing.T) {
	for _, image := range []string{
		"",
		"golang",
		"golang:latest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"golang:1.26 alpine",
		"registry.example:5000/golang",
		// Tag-only references are now rejected (mutable, drift)
		"golang:1.26-alpine",
		"golang:1.26-trixie",
		"registry.example/base/go:1.26",
	} {
		if err := ValidateRunnerImage(image); err == nil {
			t.Errorf("ValidateRunnerImage(%q) should fail", image)
		}
	}
}

func TestRunnerImageRegistryPrefixesWithoutSlash(t *testing.T) {
	for _, registry := range []string{"ghcr.io", "registry.example:5000", "registry:5000", "localhost", "localhost:5000", "docker.io"} {
		for _, suffix := range []string{"", "/"} {
			t.Run(registry+suffix, func(t *testing.T) {
				prefix := registry + suffix
				if err := ValidateRunnerImagePrefixes([]string{prefix}); err != nil {
					t.Fatal(err)
				}
				image := registry + "/org/runner:1@sha256:" + strings.Repeat("a", 64)
				if !RunnerImageAllowed(image, []string{prefix}) {
					t.Fatalf("registry prefix %q refused %q", prefix, image)
				}
				for _, other := range []string{"other.example/org/runner", registry + ".evil/org/runner"} {
					if RunnerImageAllowed(other+":1@sha256:"+strings.Repeat("a", 64), []string{prefix}) {
						t.Fatalf("registry prefix %q admitted %q", prefix, other)
					}
				}
			})
		}
	}
}

func TestRunnerImagePrefixValidatorRejectsUnparseableEntries(t *testing.T) {
	for _, prefix := range []string{"ghcr.io/Org/", "Org/runner:", "ghcr.io/org/[runner", "/", "registry.example:badport"} {
		if err := ValidateRunnerImagePrefixes([]string{prefix}); err == nil {
			t.Errorf("unparseable prefix %q accepted", prefix)
		}
	}
	// Uppercase tags are valid even though uppercase repository paths are not.
	prefix := "ghcr.io/org/runner:RC"
	if err := ValidateRunnerImagePrefixes([]string{prefix}); err != nil {
		t.Fatal(err)
	}
	if !RunnerImageAllowed("ghcr.io/org/runner:RC1@sha256:"+strings.Repeat("a", 64), []string{prefix}) {
		t.Fatal("uppercase tag prefix refused")
	}
}
