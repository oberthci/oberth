package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScannerClosesAmbientSuppressionAndPreservesVulnerabilityExit(t *testing.T) {
	dir := t.TempDir()
	scanner := filepath.Join(dir, "grype")
	// Only a synthetic external-tool fixture. No real scanner/network/credentials.
	body := "#!/bin/sh\nset -eu\ntest -z \"${GRYPE_ONLY_FIXED:-}${GRYPE_IGNORE:-}${SYFT_EXCLUDE:-}\"\ntest \"$GRYPE_DB_CACHE_DIR\" = \"$HOME/db\"\ncase \"$*\" in *'--fail-on high -o json') :;; *) exit 91;; esac\nexit 2\n"
	if err := os.WriteFile(scanner, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRYPE_ONLY_FIXED", "true")
	t.Setenv("GRYPE_IGNORE", "*")
	t.Setenv("SYFT_EXCLUDE", "*")
	before := os.Args
	defer func() { os.Args = before }()
	os.Args = []string{"scanner-driver", scanner, filepath.Join(dir, "policy.yaml"), filepath.Join(dir, "db"), filepath.Join(dir, "components.cdx.json"), "alpine:3.23.7"}
	if got := run(); got != 2 {
		t.Fatalf("scanner changed vulnerability status or leaked suppression: %d", got)
	}
	body = strings.Replace(body, "exit 2\n", "exit 9\n", 1)
	if err := os.WriteFile(scanner, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != 1 {
		t.Fatalf("scanner error became success: %d", got)
	}
}
