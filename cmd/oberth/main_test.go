package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseServeOptionsRequiresSafeComposition(t *testing.T) {
	t.Parallel()
	options, err := parseServeOptions([]string{"--argo-namespace", "argo-pipelines"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// External audit anchoring is opt-in: both URLs default to empty so a
	// fresh install contacts no external service.
	if options.runnerImagePrefixes != "" ||
		options.auditTSAURL != "" || options.auditRekorURL != "" || options.auditRekorPubKey != "" ||
		options.auditRekorInsecureHTTP ||
		options.auditAnchorInterval != 10*time.Minute || options.auditAnchorMaxAge != 30*time.Minute {
		t.Fatalf("options = %+v", options)
	}
	anchored, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--audit-tsa-url", "https://timestamp.sectigo.com/rfc3161",
		"--audit-rekor-url", "https://rekor.sigstore.dev",
		"--audit-rekor-pubkey", "/etc/oberth/audit-rekor-pubkey/rekor.pub",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if anchored.auditTSAURL != "https://timestamp.sectigo.com/rfc3161" || anchored.auditRekorURL != "https://rekor.sigstore.dev" ||
		anchored.auditRekorPubKey != "/etc/oberth/audit-rekor-pubkey/rekor.pub" {
		t.Fatalf("anchored options = %+v", anchored)
	}
	argoBase := []string{"--argo-namespace", "argo-pipelines"}
	for _, extra := range [][]string{
		{"--runner-image-prefixes", `golang":`},
		{"--runner-image-prefixes", `go\lang:`},
		{"--database", "/tmp/outside.db"},
		{"--ssh-listen", ":8443", "--https-listen", ":8443"},
		{"--audit-tsa-url", "file:///tmp/fake-tsa"},
		{"--audit-tsa-url", "http://timestamp.example.test"},
		{"--audit-tsa-url", "https:///missing-host"},
		{"--audit-tsa-url", "HTTPS://timestamp.example.test"},
		{"--audit-tsa-url", "https://user:pass@timestamp.example.test"},
		{"--audit-tsa-url", "https://timestamp.example.test/path#fragment"},
		{"--audit-tsa-url", "https://timestamp.example.test/path#"},
		{"--audit-rekor-url", "http://rekor.example.test"},
		{"--audit-rekor-url", "file:///tmp/fake-rekor"},
		{"--audit-rekor-url", "HTTPS://rekor.example.test"},
		{"--audit-rekor-url", "https://rekor.example.test/path#fragment"},
		{"--audit-rekor-url", "https://rekor.example.test/path#"},
		{"--audit-rekor-pubkey", "relative.pem"},
		{"--audit-anchor-interval", "1h", "--audit-anchor-max-age", "10m"},
		{"--audit-tsa-url", "https://timestamp.example.test", "--audit-tsa-roots", "relative.pem"},
		{"--audit-tsa-roots", "/etc/oberth/audit-tsa/roots.pem"},
		{"--audit-tsa-ca", "/etc/oberth/audit-tsa-ca/ca.crt"},
		{"--audit-rekor-ca", "/etc/oberth/audit-rekor-ca/ca.crt"},
		{"--audit-rekor-pubkey", "/etc/oberth/audit-rekor-pubkey/rekor.pub"},
		{"--accept-witness-chain-reset", strings.Repeat("a", 64)},
	} {
		arguments := append(append([]string(nil), argoBase...), extra...)
		if _, err := parseServeOptions(arguments, io.Discard); err == nil {
			t.Fatalf("unsafe arguments passed: %q", arguments)
		}
	}
}

func TestLoadRekorPublicKey(t *testing.T) {
	publicKey, err := loadRekorPublicKey("")
	if err != nil || publicKey != nil {
		t.Fatalf("empty Rekor public key = %#v, %v", publicKey, err)
	}

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rekor.pub")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	publicKey, err = loadRekorPublicKey(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, ok := publicKey.(*ecdsa.PublicKey)
	if !ok || !parsed.Equal(&privateKey.PublicKey) {
		t.Fatalf("loaded Rekor public key = %#v", publicKey)
	}

	invalidPath := filepath.Join(t.TempDir(), "invalid.pub")
	if err := os.WriteFile(invalidPath, []byte("not a PEM public key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRekorPublicKey(invalidPath); err == nil {
		t.Fatal("invalid Rekor public key loaded")
	}
}

func TestParseServeOptionsAllowsRekorHTTPOnlyWhenOptedIn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		url          string
		insecureHTTP bool
		wantError    bool
	}{
		{name: "HTTPS by default", url: "https://rekor.example.test"},
		{name: "HTTPS with opt-in", url: "https://rekor.example.test", insecureHTTP: true},
		{name: "HTTP by default", url: "http://rekor.example.test", wantError: true},
		{name: "HTTP with opt-in", url: "http://rekor.example.test", insecureHTTP: true},
		{name: "other scheme with opt-in", url: "file:///tmp/rekor", insecureHTTP: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			arguments := []string{"--argo-namespace", "argo-pipelines", "--audit-rekor-url", test.url}
			if test.insecureHTTP {
				arguments = append(arguments, "--audit-rekor-insecure-http")
			}
			options, err := parseServeOptions(arguments, io.Discard)
			if test.wantError {
				if err == nil {
					t.Fatalf("parseServeOptions(%q) succeeded", arguments)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseServeOptions(%q): %v", arguments, err)
			}
			if options.auditRekorURL != test.url || options.auditRekorInsecureHTTP != test.insecureHTTP {
				t.Fatalf("options = %+v", options)
			}
		})
	}
}
