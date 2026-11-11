package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/clientcert"
)

func TestClientRenewRotatesPairWithoutBearer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OBERTH_TOKEN", "")
	t.Setenv("OBERTH_TOKEN_COMMAND", "")
	t.Setenv("OBERTH_CLIENT_CERT", "")
	t.Setenv("OBERTH_CLIENT_KEY", "")
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, leaf, err := ca.Issue("agent", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), 0)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	initial := filepath.Join(root, "initial")
	if err := os.Mkdir(initial, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(initial, "client.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(initial, "client.crt"), cert, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(initial, ".oberth-client-identity"), []byte("oberth-client-identity-v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(root, "current")
	if err := os.Symlink(initial, current); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "" {
			t.Error("renewal sent a bearer header")
		}
		if len(r.TLS.VerifiedChains) == 0 || clientcert.Pin(r.TLS.VerifiedChains[0][0]) != clientcert.Pin(leaf) {
			t.Error("no proof with current certificate")
		}
		var request struct {
			ID     int `json:"id"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					CSR string `json:"csr"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Params.Name != "client_renew" {
			t.Error("wrong renewal tool")
		}
		issued, _, err := ca.Issue("agent", []byte(request.Params.Arguments.CSR), 0)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"structuredContent": map[string]string{"certificate": string(issued)}}})
	}))
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	serverCA := filepath.Join(root, "server-ca.crt")
	if err := os.WriteFile(serverCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runClientRenew(t.Context(), []string{"--url", server.URL, "--ca-file", serverCA, "--client-cert", filepath.Join(current, "client.crt"), "--client-key", filepath.Join(current, "client.key")}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unexpected call count", calls)
	}
	pair, err := client.LoadClientCertificate(filepath.Join(current, "client.crt"), filepath.Join(current, "client.key"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if clientcert.Pin(next) == clientcert.Pin(leaf) {
		t.Fatal("key did not rotate")
	}
	if _, err := os.Stat(filepath.Join(initial, "client.key")); err != nil {
		t.Fatal("previous key was removed")
	}
	if bytes.Contains(out.Bytes(), []byte("PRIVATE KEY")) {
		t.Fatal("renewal printed private key")
	}
}
