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
	arguments := []string{scanner, filepath.Join(dir, "policy.yaml"), filepath.Join(dir, "db"), filepath.Join(dir, "components.cdx.json"), "alpine:3.23.7"}
	if got := runScanner(arguments); got != 2 {
		t.Fatalf("scanner changed vulnerability status or leaked suppression: %d", got)
	}
	body = strings.Replace(body, "exit 2\n", "exit 9\n", 1)
	if err := os.WriteFile(scanner, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	if got := runScanner(arguments); got != 1 {
		t.Fatalf("scanner error became success: %d", got)
	}
}

func TestScannerRefusesExecutableEscapeAndUnpinnedDistro(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	scanner := filepath.Join(dir, "grype")
	if err := os.Symlink(outside, scanner); err != nil {
		t.Fatal(err)
	}
	arguments := []string{scanner, filepath.Join(dir, "config"), filepath.Join(dir, "db"), filepath.Join(dir, "sbom"), "alpine:3.23.7"}
	if got := runScanner(arguments); got != 1 {
		t.Fatalf("escaping scanner executed: %d", got)
	}
	if err := os.Remove(scanner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scanner, []byte("#!/bin/sh\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	arguments[4] = "alpine:unreviewed"
	if got := runScanner(arguments); got != 1 {
		t.Fatalf("unpinned distro accepted: %d", got)
	}
}
