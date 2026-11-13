package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/clientcert"
)

func TestPluginLocalCertificateEnrollment(t *testing.T) {
	root := t.TempDir()
	for _, shell := range trustShells() {
		t.Run(shell, func(t *testing.T) {
			dir := filepath.Join(root, filepath.Base(shell))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"env":{"KEEP":"value","NODE_EXTRA_CA_CERTS":"existing"},"model":"kept"}`), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := runTrust(t, shell, dir, nil, "keygen")
			if err != nil {
				t.Fatal(err, out)
			}
			current := filepath.Join(dir, "oberth", "current")
			keyPath := filepath.Join(current, "client.key")
			key := mustRead(t, keyPath)
			originalCSR := mustRead(t, filepath.Join(current, "client.csr"))
			if strings.Contains(out, string(key)) {
				t.Fatal("private key in output")
			}
			_, differentKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			wrongCSR, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, differentKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(current, "client.csr"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: wrongCSR}), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := runTrust(t, shell, dir, nil, "keygen"); err == nil {
				t.Fatal("reused mismatched CSR and key")
			}
			if !bytes.Equal(mustRead(t, keyPath), key) {
				t.Fatal("mismatch changed the private key")
			}
			if err := os.WriteFile(filepath.Join(current, "client.csr"), originalCSR, 0600); err != nil {
				t.Fatal(err)
			}

			if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("private key permissions", err)
			}
			if _, err := runTrust(t, shell, dir, nil, "keygen"); err != nil {
				t.Fatal(err)
			}
			if string(mustRead(t, keyPath)) != string(key) {
				t.Fatal("key overwritten")
			}
			files, err := clientcert.GenerateAuthority()
			if err != nil {
				t.Fatal(err)
			}
			ca, err := clientcert.Load(files["ca.crt"], files["ca.key"])
			if err != nil {
				t.Fatal(err)
			}
			csrPath := filepath.Join(current, "client.csr")
			csr := mustRead(t, csrPath)
			cert, leaf, err := ca.Issue("agent@host", csr, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAdminCertificate(csrPath+".crt", cert, csr); err != nil {
				t.Fatal(err)
			}
			if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
				t.Fatal("wrong usage")
			}
			if err := writeAdminCertificate(csrPath+".crt", cert, csr); err == nil {
				t.Fatal("overwrote issued certificate")
			}
			out, err = runTrust(t, shell, dir, nil, "credentials")
			if err != nil {
				t.Fatal(err, out)
			}
			var settings struct {
				Env   map[string]string `json:"env"`
				Model string            `json:"model"`
			}
			if err := json.Unmarshal(mustRead(t, filepath.Join(dir, "settings.json")), &settings); err != nil {
				t.Fatal(err)
			}
			if settings.Env["KEEP"] != "value" || settings.Env["NODE_EXTRA_CA_CERTS"] != "existing" || settings.Env["CLAUDE_CODE_CLIENT_CERT"] != csrPath+".crt" || settings.Env["CLAUDE_CODE_CLIENT_KEY"] != keyPath || settings.Model != "kept" {
				t.Fatal("settings not preserved")
			}
			if strings.Contains(out, string(key)) {
				t.Fatal("private key in output")
			}
		})
	}
}

func TestFirstAdminCSRDoesNotMintBearer(t *testing.T) {
	bootstrapCredentialInventory(t)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapPrivateKey(t, key)
	sshKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	bootstrapCredentialInventory(t, files["ca.key"])
	if *bootstrapLeakControl {
		fmt.Printf("%s", files["ca.key"])
	}
	ca, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := ca.Issue("admin@workstation", csr, 0)
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeOberthHost{t: t, uplinkOutput: string(cert)}
	var output bytes.Buffer
	deps := onboardingDeps(t, host, &output, strings.NewReader(""), false)
	path := filepath.Join(t.TempDir(), "client.csr")
	cfg := Config{AdminSSHPublicKeyPath: "-", SSHPublicKeyPath: "-", adminSSHPublicKey: ssh.MarshalAuthorizedKey(sshKey), AdminCSRPath: path, adminCSR: csr, UplinkIdentity: "admin@workstation", ClientAccess: "none"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	tw := newTableWriter(&output, false)
	tw.WriteHeader()
	tw.WriteFooter()
	creds := heldCredentials{atOnce: func(label, value string) { t.Fatal("certificate install emitted a secret credential") }}
	if err := FinishInstall(t.Context(), cfg, deps, tw, &creds); !errors.Is(err, ErrOnboardingPartial) {
		t.Fatal("expected pending forge", err)
	}
	if host.uplinkStdin != string(csr) || !strings.Contains(strings.Join(host.uplinkArgv, " "), "--csr -") || strings.Contains(strings.Join(host.uplinkArgv, " "), "--bearer") {
		t.Fatal("installer did not enroll via CSR")
	}
	if !bytes.Equal(mustRead(t, path+".crt"), cert) {
		t.Fatal("issued certificate not saved")
	}
	if strings.Contains(output.String(), "PRIVATE KEY") || strings.Contains(output.String(), "oberth_") {
		t.Fatal("secret in output")
	}
}

func TestDryRunPreservesExistingAdminOutputs(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	csrPath := filepath.Join(dir, "client.csr")
	keyPath := filepath.Join(dir, "id.pub")
	secretsPath := filepath.Join(dir, "secrets")
	for path, raw := range map[string][]byte{
		csrPath:          pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}),
		keyPath:          ssh.MarshalAuthorizedKey(sshKey),
		csrPath + ".crt": []byte("existing public certificate"),
		secretsPath:      []byte("OPENBAO_ROOT_TOKEN=s.dry-run-fixture\n"),
	} {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{DryRun: true, InstallSecretStore: true, AdminCSRPath: csrPath, AdminSSHPublicKeyPath: keyPath, SecretsFile: secretsPath}
	err = Execute(t.Context(), cfg, noClusterInstallDeps(t, false))
	if err == nil || !strings.Contains(err.Error(), "stop: cluster reached") {
		t.Fatalf("dry run refused valid existing outputs: %v", err)
	}
	for path, want := range map[string]string{csrPath + ".crt": "existing public certificate", secretsPath: "OPENBAO_ROOT_TOKEN=s.dry-run-fixture\n"} {
		if string(mustRead(t, path)) != want {
			t.Fatal("dry run changed an existing output")
		}
	}
	cfg.DryRun = false
	cfg.ResumeSecretsFile = ""
	cfg.SecretsFile = secretsPath + "-new"
	if err := Execute(t.Context(), cfg, noClusterInstallDeps(t, false)); err == nil || !strings.Contains(err.Error(), "admin certificate output already exists") {
		t.Fatalf("real install failed to protect existing certificate: %v", err)
	}
}

func TestExplicitBearerSetupClearsClientCertificate(t *testing.T) {
	for _, implementation := range append([]string{"go"}, trustShells()...) {
		t.Run(implementation, func(t *testing.T) {
			for _, env := range []string{
				`{"CLAUDE_CODE_CLIENT_CERT":"old.crt","KEEP":"value","CLAUDE_CODE_CLIENT_KEY":"old.key"}`,
				`{"KEEP":"value","CLAUDE_CODE_CLIENT_CERT":"old.crt","CLAUDE_CODE_CLIENT_KEY":"old.key"}`,
				`{"CLAUDE_CODE_CLIENT_KEY":"old.key","CLAUDE_CODE_CLIENT_CERT":"old.crt","KEEP":"value"}`,
			} {
				dir := t.TempDir()
				t.Setenv("CLAUDE_CONFIG_DIR", dir)
				path := filepath.Join(dir, "settings.json")
				if err := os.WriteFile(path, []byte(`{"env":`+env+`,"model":"kept"}`), 0600); err != nil {
					t.Fatal(err)
				}
				if implementation == "go" {
					_, err := configureClaudeMCP(t.Context(), ClaudeCodeSetup{URL: "https://new.example/mcp", Token: func(context.Context) (string, error) { return "fixture-only", nil }})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					bin := t.TempDir()
					if err := os.MkdirAll(filepath.Join(dir, "oberth"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "oberth", "ca.crt"), []byte("public CA fixture"), 0600); err != nil {
						t.Fatal(err)
					}

					if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(bin, "oberth"), []byte("#!/bin/sh\n[ \"$1 $2\" = 'mcp tools' ]\n"), 0700); err != nil {
						t.Fatal(err)
					}
					if out, err := runTrust(t, implementation, dir, []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}, "configure", "https://new.example/mcp", "/tmp/fixture-token"); err != nil {
						t.Fatal(err, out)
					}
				}
				var result struct {
					Env   map[string]string `json:"env"`
					Model string            `json:"model"`
				}
				if err := json.Unmarshal(mustRead(t, path), &result); err != nil {
					t.Fatal(err)
				}
				if _, ok := result.Env["CLAUDE_CODE_CLIENT_CERT"]; ok {
					t.Fatal("bearer setup retained certificate")
				}
				if _, ok := result.Env["CLAUDE_CODE_CLIENT_KEY"]; ok {
					t.Fatal("bearer setup retained key")
				}
				if result.Env["KEEP"] != "value" || result.Model != "kept" {
					t.Fatal("other settings were changed")
				}
			}
		})
	}
}
