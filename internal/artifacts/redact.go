// Package artifacts collects bounded run artifacts and refuses secret material.
// Every complete member is scanned before any member is written. Matching files
// are refused rather than rewritten, preserving artifact hashes and semantics.
package artifacts

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// ErrSecretDetected is returned when a secret pattern is found in an artifact.
var ErrSecretDetected = errors.New("artifact contains secret material")

// scanBytesForSecrets checks the complete member already held by Extract's
// bounded staging buffer. There is no independent scan ceiling or line limit,
// and patterns may include newlines. Errors never include the secret value.
func scanBytesForSecrets(body []byte, patterns []string) error {
	for _, pattern := range patterns {
		if pattern != "" && bytes.Contains(body, []byte(pattern)) {
			return fmt.Errorf("%w: matched pattern of length %d", ErrSecretDetected, len(pattern))
		}
	}
	return nil
}

// TierGated reports whether artifact collection should be enabled for the
// given trigger. By default, only CI-trigger (branch) runs collect artifacts;
// credentialed tiers (release, plan, apply) are excluded to preserve the
// memory-only invariant for secret material. An operator may override this
// with an explicit opt-in per repository.
func TierGated(trigger string) bool {
	switch strings.ToLower(trigger) {
	case "ci":
		return true
	default:
		// release, plan, apply, and any future credentialed trigger are
		// excluded by default.
		return false
	}
}

// DefaultScanPatterns is the structural secret-material set persisting call
// sites pass to Extract. On the Argo path the server deliberately never holds
// a run's secret values — CI-tier pods fetch upstream-scoped secrets from the
// store themselves, so a value-based redaction list is empty on exactly the
// tier that collects artifacts. What the server CAN refuse structurally is
// key material: PEM private-key headers appear verbatim both in raw key files
// and embedded inside JSON service-account keys, and are never a legitimate
// line in a test report, coverage tree, or build log. A fixture key tripping
// this gate refuses that run's collection with the member named — an honest,
// bounded false positive, preferred over persisting a real key to /data.
var DefaultScanPatterns = []string{
	"-----BEGIN OPENSSH PRIVATE KEY-----",
	"-----BEGIN PRIVATE KEY-----",
	"-----BEGIN RSA PRIVATE KEY-----",
	"-----BEGIN EC PRIVATE KEY-----",
	"-----BEGIN DSA PRIVATE KEY-----",
	"-----BEGIN ENCRYPTED PRIVATE KEY-----",
	"-----BEGIN ENCRYPTED COSIGN PRIVATE KEY-----",
	"-----BEGIN ENCRYPTED SIGSTORE PRIVATE KEY-----",
	"-----BEGIN PGP PRIVATE KEY BLOCK-----",
}
