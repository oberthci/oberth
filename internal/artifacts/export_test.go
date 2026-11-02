package artifacts

import (
	"fmt"
	"io"
	"os"
)

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
