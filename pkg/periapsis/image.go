package periapsis

import (
	"fmt"
	"strings"
)

const (
	// maxRunnerImageBytes bounds one declared image reference.
	maxRunnerImageBytes = 300
)

// DefaultRunnerImagePrefixes is the allowlist applied when the administrator
// configures no explicit --runner-image-prefixes. It must stay in sync with
// the chart default in charts/oberth/values.yaml; a cross-source drift test
// enforces the invariant.
var DefaultRunnerImagePrefixes = []string{
	"golang:", "debian:", "aquasec/trivy:",
	// Node and Maven, because `oberth init` generates pipelines that run
	// them. A runner cannot install a toolchain at build time under a
	// non-root read-only rootfs, so the image has to carry one, and a prefix
	// that is not allowed here is a generated pipeline that cannot start.
	"node:", "maven:",
}

// ValidateRunnerImage enforces the syntax shared by repository declarations
// and the admission gate. Every runner image reference MUST contain a
// validated @sha256:<64 lowercase hex> digest; a human-readable tag may
// precede the digest. Tag-only references are rejected because a registry
// writer can move an allowed tag between admission and node pull, breaking
// the immutability contract. The administrator prefix allowlist -- not this
// syntactic check -- decides which registries and repositories are trusted.
func ValidateRunnerImage(image string) error {
	if image == "" || len(image) > maxRunnerImageBytes {
		return fmt.Errorf("runner image must contain 1-%d bytes", maxRunnerImageBytes)
	}
	if strings.TrimSpace(image) != image || strings.ContainsAny(image, " \t\x00\r\n\"'\\") {
		return fmt.Errorf("runner image %q must not contain whitespace, quotes, or control characters", image)
	}
	name := image
	atIndex := strings.Index(name, "@")
	if atIndex < 0 {
		return fmt.Errorf("runner image %q must contain an @sha256:<64 hex> digest; tag-only references are mutable and rejected at admission", image)
	}
	digest := name[atIndex+1:]
	name = name[:atIndex]
	if !validImageDigest(digest) {
		return fmt.Errorf("runner image %q digest must be sha256:<64 lowercase hex>", image)
	}
	// A tag before the digest is optional but, when present, must be valid.
	lastColon := strings.LastIndex(name, ":")
	lastSlash := strings.LastIndex(name, "/")
	if lastColon >= 0 && lastColon > lastSlash {
		tag := name[lastColon+1:]
		if tag == "" || len(tag) > 128 || !validImageTag(tag) {
			return fmt.Errorf("runner image %q tag is invalid", image)
		}
		if tag == "latest" {
			return fmt.Errorf("runner image %q must not use the mutable %q tag", image, "latest")
		}
		name = name[:lastColon]
	}
	if name == "" {
		return fmt.Errorf("runner image %q repository is empty", image)
	}
	return nil
}

// RunnerImageAllowed reports whether image starts with one of the
// administrator-permitted prefixes. An empty allowlist permits only the
// built-in DefaultRunnerImagePrefixes.
func RunnerImageAllowed(image string, prefixes []string) bool {
	if len(prefixes) == 0 {
		prefixes = DefaultRunnerImagePrefixes
	}
	for _, prefix := range prefixes {
		if prefix != "" && strings.HasPrefix(image, prefix) {
			return true
		}
	}
	return false
}

// ValidateRunnerImagePrefixes rejects malformed administrator allowlist
// entries before they can silently allow nothing or everything.
func ValidateRunnerImagePrefixes(prefixes []string) error {
	seen := make(map[string]struct{}, len(prefixes))
	for _, prefix := range prefixes {
		if prefix == "" || strings.TrimSpace(prefix) != prefix || strings.ContainsAny(prefix, " \t\x00\r\n\"'\\") {
			return fmt.Errorf("runner image prefix %q must be a clean nonempty string", prefix)
		}
		if len(prefix) > maxRunnerImageBytes {
			return fmt.Errorf("runner image prefix %q exceeds %d bytes", prefix, maxRunnerImageBytes)
		}
		if _, duplicate := seen[prefix]; duplicate {
			return fmt.Errorf("runner image prefix %q is duplicated", prefix)
		}
		seen[prefix] = struct{}{}
	}
	return nil
}

func validImageDigest(digest string) bool {
	hexPart, found := strings.CutPrefix(digest, "sha256:")
	if !found || len(hexPart) != 64 {
		return false
	}
	for _, character := range []byte(hexPart) {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validImageTag(tag string) bool {
	for index, character := range []byte(tag) {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9', character == '_':
		case character == '.' || character == '-':
			if index == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
