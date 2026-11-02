package pipeline

import (
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

const (
	// maxRunnerImageBytes bounds one declared image reference.
	maxRunnerImageBytes = 300
)

// DefaultRunnerImagePrefixes is the allowlist applied when the administrator
// configures no explicit --runner-image-prefixes. It must stay in sync with
// the chart default in charts/oberth/values.yaml; a cross-source drift test
// enforces the invariant.
var DefaultRunnerImagePrefixes = []string{"golang:", "debian:", "aquasec/trivy:"}

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

// RunnerImageAllowed compares normalized registries and repository boundaries.
// Tags may still be narrowed by a tag prefix, but a registry port must never
// be mistaken for a tag. Docker Hub short and fully qualified names agree.
func RunnerImageAllowed(image string, prefixes []string) bool {
	reference, err := name.ParseReference(image)
	if err != nil {
		return false
	}
	base, digest, _ := strings.Cut(image, "@")
	canonical := reference.Context().Name()
	// The name package treats bare localhost as a Docker Hub namespace.
	// Docker's reference syntax reserves it as an explicit registry instead.
	if strings.HasPrefix(image, "localhost/") {
		canonical = strings.TrimPrefix(canonical, name.DefaultRegistry+"/")
	}
	if colon := strings.LastIndex(base, ":"); colon > strings.LastIndex(base, "/") {
		canonical += base[colon:]
	}
	if digest != "" {
		canonical += "@" + digest
	}
	if len(prefixes) == 0 {
		prefixes = DefaultRunnerImagePrefixes
	}
	for _, prefix := range prefixes {
		normalized, err := normalizedImagePrefix(prefix)
		if err != nil || !strings.HasPrefix(canonical, normalized) {
			continue
		}
		// A tag/digest prefix or a namespace ending in / is already bounded.
		// A bare repository/namespace must end at a path or reference boundary.
		if strings.HasSuffix(normalized, "/") || strings.Contains(normalized[strings.LastIndex(normalized, "/")+1:], ":") || strings.Contains(normalized, "@") ||
			len(canonical) == len(normalized) || strings.ContainsAny(canonical[len(normalized):len(normalized)+1], "/:@") {
			return true
		}
	}
	return false
}

func normalizedImagePrefix(prefix string) (string, error) {
	base, digest, hasDigest := strings.Cut(prefix, "@")
	registryName := strings.TrimSuffix(base, "/")
	host, port, hasPort := strings.Cut(registryName, ":")
	// Preserve repository tag prefixes such as golang: and golang:1.26.
	// A numeric port also identifies a registry without a dotted hostname.
	numericPort := hasPort && port != "" && strings.Trim(port, "0123456789") == ""
	if !hasDigest && !strings.Contains(registryName, "/") && (strings.Contains(host, ".") || host == "localhost" || numericPort) {
		registry, err := name.NewRegistry(registryName)
		if err != nil {
			return "", err
		}
		return registry.Name() + "/", nil
	}
	suffix := ""
	if colon := strings.LastIndex(base, ":"); colon > strings.LastIndex(base, "/") {
		suffix, base = base[colon:], base[:colon]
	}
	if hasDigest {
		suffix += "@" + digest
	}
	if strings.HasSuffix(base, "/") {
		base = strings.TrimSuffix(base, "/")
		if base == "" {
			return "", fmt.Errorf("repository or registry is required")
		}
		suffix = "/" + suffix
		if !strings.Contains(base, "/") && strings.ContainsAny(base, ".:") {
			registry, err := name.NewRegistry(base)
			if err != nil {
				return "", err
			}
			return registry.Name() + suffix, nil
		}
		// A trailing slash denotes a namespace, not an official image in
		// Docker Hub's implicit library namespace (for example aquasec/).
		repository, err := name.NewRepository(base + "/prefix-placeholder")
		if err != nil {
			return "", err
		}
		normalized := strings.TrimSuffix(repository.Name(), "/prefix-placeholder")
		if strings.HasPrefix(base, "localhost/") {
			normalized = strings.TrimPrefix(normalized, name.DefaultRegistry+"/")
		}
		return normalized + suffix, nil
	}
	repository, err := name.NewRepository(base)
	if err != nil {
		return "", err
	}
	normalized := repository.Name()
	if strings.HasPrefix(base, "localhost/") {
		normalized = strings.TrimPrefix(normalized, name.DefaultRegistry+"/")
	}
	return normalized + suffix, nil
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
		if _, err := normalizedImagePrefix(prefix); err != nil {
			return fmt.Errorf("runner image prefix %q is invalid: %w", prefix, err)
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
