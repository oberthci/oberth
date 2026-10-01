// Package artifacts provides the redaction gate for run artifact collection.
//
// When PR#16 (run artifacts) merges, every file collected from a pipeline's
// $OBERTH_ARTIFACTS directory must pass through ScanForSecrets before it is
// persisted to the server's /data PVC. This package exists as a pre-merge
// hardening measure: the scanning infrastructure is reviewed and tested
// independently, so the artifact PR can import it as a dependency rather
// than building redaction in-band.
//
// The redaction set is the same configured-secret set the log writer uses
// (internal/redact), ensuring that any value the log masks is also caught
// in an artifact. A file that matches is refused — fail-closed, not
// redact-in-place — because silently rewriting artifact contents would
// create a correctness hazard (test reports, coverage data, plan files).
package artifacts

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxScanBytes bounds the prefix of each file scanned for secrets. Files
// larger than this are scanned only in their first maxScanBytes; the
// alternative (refusing all large files) would break legitimate large
// artifacts like coverage databases.
const maxScanBytes = 64 << 20 // 64 MiB

// ScanForSecrets reads a file and reports whether it contains any of the
// given secret patterns. It returns a non-nil error naming the matched
// pattern index (not the value) if a match is found. An empty pattern
// list always passes. The file is read in streaming fashion and only the
// first maxScanBytes are scanned.
//
// The patterns are the same redaction set the log masking writer uses;
// callers should pass the server's configured redact list.
func ScanForSecrets(path string, patterns []string) error {
	if len(patterns) == 0 {
		return nil
	}
	// Filter out empty patterns (same guard as redactOutput in command.go).
	var active [][]byte
	for _, p := range patterns {
		if p != "" {
			active = append(active, []byte(p))
		}
	}
	if len(active) == 0 {
		return nil
	}
	// path is always under the server's controlled artifact staging directory;
	// it is never user-supplied input.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return fmt.Errorf("artifact redaction scan: %w", err)
	}
	defer func() { _ = file.Close() }()
	return scanReader(io.LimitReader(file, maxScanBytes), active)
}

// ScanReaderForSecrets is the io.Reader variant for testing and for scanning
// in-memory buffers (e.g. tar entries before extraction).
func ScanReaderForSecrets(r io.Reader, patterns []string) error {
	var active [][]byte
	for _, p := range patterns {
		if p != "" {
			active = append(active, []byte(p))
		}
	}
	if len(active) == 0 {
		return nil
	}
	return scanReader(io.LimitReader(r, maxScanBytes), active)
}

// ErrSecretDetected is returned when a secret pattern is found in an artifact.
var ErrSecretDetected = errors.New("artifact contains secret material")

func scanReader(r io.Reader, patterns [][]byte) error {
	// Use a scanner with a large buffer to handle long lines. The scan is
	// line-oriented so partial matches across line boundaries are not a
	// concern for the typical secret patterns (tokens, keys, passwords)
	// which are single-line values.
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 256<<10), 1<<20) // 1 MiB max line
	for scanner.Scan() {
		line := scanner.Bytes()
		for _, pattern := range patterns {
			if bytes.Contains(line, pattern) {
				return fmt.Errorf("%w: matched pattern of length %d", ErrSecretDetected, len(pattern))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("artifact redaction scan: %w", err)
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
