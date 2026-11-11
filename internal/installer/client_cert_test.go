package installer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
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
			if strings.Contains(out, string(key)) {
				t.Fatal("private key in output")
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
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
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
