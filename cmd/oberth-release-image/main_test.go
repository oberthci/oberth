package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRunRejectsUnboundReleaseInputBeforeRegistryAccess(t *testing.T) {
	if err := run(context.Background(), []string{"publish", "not-semver", "bad", "bad", t.TempDir(), "missing"}); err == nil {
		t.Fatal("invalid release input was accepted")
	}
	if err := run(context.Background(), []string{"unknown", "v1.2.3", "0123456789abcdef0123456789abcdef01234567", "2026-08-06T10:00:00Z", t.TempDir(), "missing"}); err == nil {
		t.Fatal("unknown image action was accepted")
	}
}

func TestReadReferenceRequiresOneCanonicalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.txt")
	want := "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(path, []byte(want+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readReference(path); err != nil || got != want {
		t.Fatalf("reference = %q, %v", got, err)
	}
	if err := os.WriteFile(path, []byte(want+"\nextra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReference(path); err == nil {
		t.Fatal("multi-line image reference was accepted")
	}
}

func TestStaticSBOMOutputConfinesCanonicalFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "static-git-amd64.cdx.json")
	if err := run(t.Context(), []string{"static-sbom", "amd64", path}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"static-sbom", "amd64", path}); err == nil {
		t.Fatal("SBOM output escaped through a symlink")
	}
	body, err := os.ReadFile(outside)
	if err != nil || string(body) != "unchanged" {
		t.Fatalf("outside file changed: %q, %v", body, err)
	}
	for _, arguments := range [][]string{
		{"static-sbom", "amd64", filepath.Join(dir, "other.json")},
		{"static-sbom", "other", path},
		{"static-sbom", "amd64", dir + "/../static-git-amd64.cdx.json"},
	} {
		if err := run(t.Context(), arguments); err == nil {
			t.Fatalf("invalid SBOM output accepted: %v", arguments)
		}
	}
}
