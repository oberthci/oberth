package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/releaseverify"
)

func TestDownloadedVerificationFailureStopsBeforeOutputs(t *testing.T) {
	files, public := signedDownloads(t)
	files["SHA256SUMS.sigstore.json"] = []byte(`{"messageSignature":{"signature":"YmFk"}}`)
	env := cleanScriptEnvironment(t, files, public)
	configureDownloadOrigin(t, files, env, "")
	bundle := filepath.Join(env["RELEASE_DIR"], "SHA256SUMS.sigstore.json")
	if err := os.WriteFile(bundle, []byte("local signature sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := releaseScript(t, "verify", env)
	want := releaseverify.Signature.Error() + "\n" + releaseverify.Signature.Remedy() + "\n"
	if err == nil || string(output) != want {
		t.Fatal("verification did not refuse", err, string(output))
	}
	body, err := os.ReadFile(bundle)
	if err != nil || string(body) != "local signature sentinel" {
		t.Fatal("failed verification replaced the local bundle", err)
	}
	if _, err := os.Stat(filepath.Join(env["RELEASE_SHARED"], "verified.json")); !os.IsNotExist(err) {
		t.Fatal("failed signature created a receipt", err)
	}
}
