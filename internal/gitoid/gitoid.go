// Package gitoid provides shared Git object-ID and workspace-path validation
// helpers used across the store, service, and command layers.
package gitoid

import (
	"path/filepath"
	"strings"
)

// Valid reports whether value is a well-formed lowercase hex object ID
// (40-character SHA-1 or 64-character SHA-256) without trimming whitespace.
// Store-layer callers that persist already-validated OIDs use this form.
func Valid(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// ValidTrimmed reports whether value, after leading/trailing whitespace
// removal and lowercase normalization, is a well-formed hex object ID.
// Service-boundary callers that accept external input use this form.
func ValidTrimmed(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range strings.ToLower(value) {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// Same reports whether two object IDs refer to the same object, ignoring
// case and surrounding whitespace.
func Same(first, second string) bool {
	return strings.EqualFold(strings.TrimSpace(first), strings.TrimSpace(second))
}

// PathWithin reports whether path is contained inside root using filepath.Rel.
func PathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// StrictPathWithin reports whether path is an absolute, already-clean path
// contained inside root.
func StrictPathWithin(root, path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	return PathWithin(root, path)
}
