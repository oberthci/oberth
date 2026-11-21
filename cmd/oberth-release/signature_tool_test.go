package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func compiledReleaseTool(t *testing.T, public []byte) string {
	t.Helper()
	dir := t.TempDir()
	pin := filepath.Join(dir, "fixture.pub")
	if err := os.WriteFile(pin, public, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := filepath.Abs("../../internal/releaseverify/release-cosign.pub")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"Replace": map[string]string{source: pin}})
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "overlay.json")
	if err = os.WriteFile(overlay, data, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "oberth-release")
	command := exec.CommandContext(t.Context(), "go", "build", "-p=2", "-buildvcs=false", "-overlay="+overlay, "-o", binary, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	return binary
}

func TestSignatureCommandUsesCompiledKeyAndNoReleaseRecord(t *testing.T) {
	files, public := signedDownloads(t)
	dir := t.TempDir()
	for _, name := range []string{"SHA256SUMS", "SHA256SUMS.sigstore.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0600); err != nil {
			t.Fatal(err)
		}
	}
	tool := compiledReleaseTool(t, public)
	command := exec.CommandContext(t.Context(), tool, "signature", "--dir", dir)
	if output, err := command.CombinedOutput(); err != nil || string(output) != "Release signature verified.\n" {
		t.Fatal("key-only signature did not verify", err, string(output))
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	command = exec.CommandContext(t.Context(), tool, "signature", "--dir", dir)
	if err := command.Run(); err == nil {
		t.Fatal("tampered checksum list accepted")
	}
	_, other := signedDownloads(t)
	candidate := filepath.Join(dir, "candidate.pub")
	if err := os.WriteFile(candidate, other, 0600); err != nil {
		t.Fatal(err)
	}
	command = exec.CommandContext(t.Context(), tool, "key", "--public-key-file", candidate)
	if err := command.Run(); err == nil {
		t.Fatal("unrelated public key accepted")
	}
	if err := os.WriteFile(candidate, public, 0600); err != nil {
		t.Fatal(err)
	}
	command = exec.CommandContext(t.Context(), tool, "key", "--public-key-file", candidate)
	if err := command.Run(); err != nil {
		t.Fatal("matching public key refused", err)
	}
}
