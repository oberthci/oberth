package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run hold patch bounds the diff before it is read whole and encoded: a
// diff at the limit is read, one byte more is refused, from a file and from
// standard input alike; a path that is not a regular file is refused.
func TestReadHoldDiffBoundsSizeBeforeEncoding(t *testing.T) {
	const limit = 64
	diff := strings.Repeat("x", limit)
	if got, err := readHoldDiff(strings.NewReader(diff), limit); err != nil || string(got) != diff {
		t.Fatalf("diff at the limit = %d bytes, %v", len(got), err)
	}
	if _, err := readHoldDiff(strings.NewReader(diff+"y"), limit); !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "larger than 64 bytes") {
		t.Fatalf("oversized stdin diff = %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "change.diff")
	if err := os.WriteFile(path, []byte(diff), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := holdDiffFileWithinLimit(path, limit); err != nil {
		t.Fatalf("diff file at the limit = %v", err)
	}
	if err := os.WriteFile(path, []byte(diff+"y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := holdDiffFileWithinLimit(path, limit); !errors.Is(err, errUsage) {
		t.Fatalf("oversized file diff = %v", err)
	}
	if err := holdDiffFileWithinLimit(dir, limit); !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory as diff file = %v", err)
	}
	if err := holdDiffFileWithinLimit(filepath.Join(dir, "missing"), limit); err == nil {
		t.Fatal("a missing diff file passed")
	}
}
