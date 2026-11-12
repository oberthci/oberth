package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/invite"
)

func TestUplinkInviteWritesPrivateHandoffWithoutPrintingSecret(t *testing.T) {
	isolateRemoteConfig(t)
	secret, _, err := invite.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tokenPath := filepath.Join(root, "admin-token")
	if err := os.WriteFile(tokenPath, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("admin credential was not supplied")
		}
		var request struct {
			ID     int `json:"id"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					Identity     string `json:"identity"`
					ServerURL    string `json:"server_url"`
					SecretDigest string `json:"secret_digest"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error("invalid admin request")
			return
		}
		if request.Params.Name != "uplink_invite" || invite.ValidatePin(request.Params.Arguments.SecretDigest) != nil {
			t.Error("invite request did not carry only a digest")
		}
		binding := invite.Binding{ID: "public-id", Identity: request.Params.Arguments.Identity, ServerURL: request.Params.Arguments.ServerURL, CAPin: strings.Repeat("a", 64), ExpiresAt: time.Now().Add(time.Hour)}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"structuredContent": binding}})
	}))
	identity, serverCA := inviteCLIIdentity(t)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}}
	server.StartTLS()
	server.URL = strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	defer server.Close()
	caPath := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCA.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(root, "invite.json")
	var output bytes.Buffer
	if err := runUplinkInvite(t.Context(), []string{"agent@example.test", "--out", outPath, "--url", server.URL, "--ca-file", caPath, "--token-file", tokenPath}, &output); err != nil {
		t.Fatal(err)
	}
	handed, err := readPrivateInvite(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if handed.Identity != "agent@example.test" || strings.Contains(output.String(), handed.Secret) || strings.Contains(output.String(), secret) {
		t.Fatal("invite output leaked a credential or changed identity")
	}
	info, err := os.Stat(outPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe invite file mode", err)
	}
	if err := runUplinkInvite(t.Context(), []string{"agent@example.test", "--out", outPath, "--url", server.URL, "--ca-file", caPath, "--token-file", tokenPath}, &output); err == nil {
		t.Fatal("existing invite file overwritten")
	}
}
func isolateRemoteConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND", "OBERTH_CLIENT_CERT", "OBERTH_CLIENT_KEY", "OBERTH_BASE_URL", "OBERTH_MCP_URL", "OBERTH_CA_CERT"} {
		t.Setenv(name, "")
	}
}
func inviteCLIIdentity(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Invite test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	raw, err := x509.CreateCertificate(rand.Reader, root, root, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, leafKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"localhost"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, leafKey.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, raw}, PrivateKey: leafKey}, ca
}
func TestClientRedeemPinsBeforeSendingAndKeepsSecretsLocal(t *testing.T) {
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	identity, serverCA := inviteCLIIdentity(t)
	digest := sha256.Sum256(serverCA.Raw)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	sshPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(sshPublic)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := invite.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/mcp/enroll" || r.Header.Get("Authorization") != "" {
			t.Error("redemption used the wrong transport")
		}
		var request struct {
			Params struct {
				Arguments struct {
					Secret string `json:"secret"`
					CSR    string `json:"csr"`
					SSHKey string `json:"ssh_public_key"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error("invalid redemption request")
			return
		}
		if request.Params.Arguments.Secret != secret {
			t.Error("wrong one-time credential")
		}
		if request.Params.Arguments.CSR != string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})) || request.Params.Arguments.SSHKey != string(ssh.MarshalAuthorizedKey(public)) {
			t.Error("enrollment transmitted data outside canonical public inputs")
		}
		certificate, _, err := issuer.Issue("agent@example.test", []byte(request.Params.Arguments.CSR), 0)
		if err != nil {
			t.Error("cannot issue test certificate")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"structuredContent": invite.Certificate{Identity: "agent@example.test", Certificate: string(certificate), SSHFingerprint: ssh.FingerprintSHA256(public)}}})
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}}
	server.StartTLS()
	server.URL = strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	defer server.Close()
	root := t.TempDir()
	path := filepath.Join(root, "invite.json")
	csrPath := filepath.Join(root, "client.csr")
	sshPath := filepath.Join(root, "push.pub")
	if err := os.WriteFile(csrPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshPath, ssh.MarshalAuthorizedKey(public), 0600); err != nil {
		t.Fatal(err)
	}
	envelope := invite.File{Binding: invite.Binding{Identity: "agent@example.test", ServerURL: server.URL + "/mcp", CAPin: strings.Repeat("f", 64), ExpiresAt: time.Now().Add(time.Hour)}, Secret: secret}
	write := func() {
		t.Helper()
		body, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	var output bytes.Buffer
	out := filepath.Join(root, "client.crt")
	arguments := []string{"--invite-file", path, "--csr", csrPath, "--ssh-public-key", sshPath, "--certificate-out", out}
	if err := runClientRedeem(t.Context(), arguments, &output); err == nil || calls.Load() != 0 {
		t.Fatal("credential sent before CA pin validation")
	}
	envelope.CAPin = hex.EncodeToString(digest[:])
	write()
	privateBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateBytes}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runClientRedeem(t.Context(), arguments, &output); err == nil || calls.Load() != 0 {
		t.Fatal("private SSH material was sent as a public key")
	}
	for _, bad := range [][]byte{
		append([]byte("-----BEGIN PRIVATE KEY-----\nprivate-data\n-----END PRIVATE KEY-----\n"), ssh.MarshalAuthorizedKey(public)...),
		append(ssh.MarshalAuthorizedKey(public), []byte("-----BEGIN PRIVATE KEY-----\nprivate-data\n-----END PRIVATE KEY-----\n")...),
	} {
		if err := os.WriteFile(sshPath, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if err := runClientRedeem(t.Context(), arguments, &output); err == nil || calls.Load() != 0 {
			t.Fatal("mixed SSH input was transmitted")
		}
	}
	if err := os.WriteFile(sshPath, append(bytes.TrimSpace(ssh.MarshalAuthorizedKey(public)), []byte(" private-comment-must-stay-local\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(csrPath, append([]byte("private-prefix-must-stay-local\n"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runClientRedeem(t.Context(), arguments, &output); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || strings.Contains(output.String(), secret) || strings.Contains(output.String(), "PRIVATE KEY") {
		t.Fatal("redemption leaked a secret or retried")
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe client certificate mode", err)
	}
	if err := runClientRedeem(t.Context(), arguments, &output); err == nil || calls.Load() != 1 {
		t.Fatal("existing output consumed another invitation")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateInvite(path); err == nil {
		t.Fatal("world-readable invitation accepted")
	}
}
