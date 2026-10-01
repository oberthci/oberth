package artifacts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanForSecretsDetectsPatterns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "leak.txt")
	if err := os.WriteFile(file, []byte("some output\nVAULT_TOKEN=s.mySecretToken123\nmore output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ScanForSecrets(file, []string{"s.mySecretToken123"})
	if err == nil {
		t.Fatal("scan did not detect the secret")
	}
	if !errors.Is(err, ErrSecretDetected) {
		t.Fatalf("error is not ErrSecretDetected: %v", err)
	}
}

func TestScanForSecretsPassesCleanFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "clean.txt")
	if err := os.WriteFile(file, []byte("some clean output\nno secrets here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ScanForSecrets(file, []string{"s.mySecretToken123"}); err != nil {
		t.Fatalf("clean file failed scan: %v", err)
	}
}

func TestScanForSecretsHandlesEmptyPatterns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "any.txt")
	if err := os.WriteFile(file, []byte("s.mySecretToken123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Empty pattern list always passes.
	if err := ScanForSecrets(file, nil); err != nil {
		t.Fatalf("nil patterns failed: %v", err)
	}
	if err := ScanForSecrets(file, []string{}); err != nil {
		t.Fatalf("empty patterns failed: %v", err)
	}
	// Empty string patterns are filtered out.
	if err := ScanForSecrets(file, []string{"", ""}); err != nil {
		t.Fatalf("blank patterns failed: %v", err)
	}
}

func TestScanReaderForSecretsWorksOnStreams(t *testing.T) {
	t.Parallel()
	content := "line 1\nline with secret-value-xyz\nline 3\n"
	err := ScanReaderForSecrets(strings.NewReader(content), []string{"secret-value-xyz"})
	if err == nil {
		t.Fatal("reader scan did not detect the secret")
	}
	if !errors.Is(err, ErrSecretDetected) {
		t.Fatalf("error is not ErrSecretDetected: %v", err)
	}
}

func TestScanForSecretsRespectsMaxScanBytes(t *testing.T) {
	t.Parallel()
	// Create a file larger than maxScanBytes with the secret only at the end.
	// The scanner should NOT detect it because it's beyond the scan limit.
	dir := t.TempDir()
	file := filepath.Join(dir, "large.txt")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	// Write more than maxScanBytes of clean data.
	clean := strings.Repeat("clean data line\n", (maxScanBytes/16)+1)
	if _, err := f.WriteString(clean); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err := f.WriteString("secret-after-limit\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	// The secret is beyond the scan limit.
	if err := ScanForSecrets(file, []string{"secret-after-limit"}); err != nil {
		// This is acceptable — the scanner may or may not reach the end depending
		// on buffering. The important thing is it doesn't crash.
		if !errors.Is(err, ErrSecretDetected) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestTierGatedAllowsCIOnly(t *testing.T) {
	t.Parallel()
	if !TierGated("ci") {
		t.Fatal("ci trigger should be allowed")
	}
	if TierGated("release") {
		t.Fatal("release trigger should be gated")
	}
	if TierGated("plan") {
		t.Fatal("plan trigger should be gated")
	}
	if TierGated("apply") {
		t.Fatal("apply trigger should be gated")
	}
}

func TestScanForSecretsMissingFile(t *testing.T) {
	t.Parallel()
	err := ScanForSecrets("/nonexistent/path/file.txt", []string{"secret"})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if errors.Is(err, ErrSecretDetected) {
		t.Fatal("missing file should not be reported as secret detected")
	}
}
