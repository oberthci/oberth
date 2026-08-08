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
	want := "europe-west4-docker.pkg.dev/skipopsmain/cloudtaser/cloudtaser-oberth@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
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
