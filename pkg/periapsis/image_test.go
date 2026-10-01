package periapsis

import (
	"testing"
)

func TestRunnerImageAllowedAgainstPrefixes(t *testing.T) {
	if !RunnerImageAllowed("golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil) {
		t.Fatal("empty allowlist must admit the built-in golang: prefix")
	}
	if RunnerImageAllowed("evil.example/golang:1.26@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil) {
		t.Fatal("foreign registry admitted by the default prefix")
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
		"golang:1.26-trixie@sha256:ab563819a16cfe5faff0f96a8bb598fbb0e400ab2ac751996e60abcb23b106a3",
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
