package service

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
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/invite"
)

func inviteServerIdentity(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	_, rootKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Enrollment test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour)}
	rootDER, err := x509.CreateCertificate(rand.Reader, template, template, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	_, leafKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, leaf, root, leafKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{raw, rootDER}, PrivateKey: leafKey}, root
}
func TestInviteRedemptionOverMCPAndMutualTLS(t *testing.T) {
	fixture := newScopeFixture(t)
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	authority, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	identity, root := inviteServerIdentity(t)
	digest := sha256.Sum256(root.Raw)
	pin := hex.EncodeToString(digest[:])
	fixture.service.clientCertificates = &ClientCertIssuer{Authority: authority, Store: fixture.store, ServerCAPin: pin}
	authenticator, err := NewAuthenticator(stubUplinkAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	authenticator.Certificates = fixture.store
	control, err := api.New(authenticator, fixture.service, fixture.service, "test")
	if err != nil {
		t.Fatal(err)
	}
	var enrollmentRequests atomic.Int32
	var redirect atomic.Bool
	var redirected atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer trap.Close()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp/enroll" {
			enrollmentRequests.Add(1)
			if redirect.Load() {
				http.Redirect(w, r, trap.URL, http.StatusTemporaryRedirect)
				return
			}
		}
		control.Handler().ServeHTTP(w, r)
	}))
	pool := x509.NewCertPool()
	pool.AddCert(authority.Certificate)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	secret, hash, err := invite.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	value, err := fixture.call(unscopedActor, "uplink_invite", map[string]any{"identity": "agent@example.test", "server_url": server.URL + "/mcp", "secret_digest": hash, "scope": []string{"github"}})
	if err != nil {
		t.Fatal(err)
	}
	envelope := invite.File{Binding: value.(invite.Binding), Secret: secret}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rawCSR, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: rawCSR}))
	sshPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(sshPublic)
	if err != nil {
		t.Fatal(err)
	}
	wrong := envelope
	wrong.CAPin = strings.Repeat("f", 64)
	if _, err := invite.Redeem(t.Context(), wrong, csr, string(ssh.MarshalAuthorizedKey(sshKey))); err == nil {
		t.Fatal("wrong CA pin accepted")
	}
	if enrollmentRequests.Load() != 0 {
		t.Fatal("credential sent before checking the CA pin")
	}
	wrong = envelope
	wrong.ServerURL = strings.Replace(envelope.ServerURL, "127.0.0.1", "localhost", 1)
	if _, err := invite.Redeem(t.Context(), wrong, csr, string(ssh.MarshalAuthorizedKey(sshKey))); err == nil {
		t.Fatal("hostname mismatch accepted")
	}
	if enrollmentRequests.Load() != 0 {
		t.Fatal("credential sent before checking the hostname")
	}
	redirect.Store(true)
	if _, err := invite.Redeem(t.Context(), envelope, csr, string(ssh.MarshalAuthorizedKey(sshKey))); err == nil {
		t.Fatal("redirected enrollment accepted")
	}
	if redirected.Load() != 0 {
		t.Fatal("invite credential followed a redirect")
	}
	redirect.Store(false)
	issued, err := invite.Redeem(t.Context(), envelope, csr, string(ssh.MarshalAuthorizedKey(sshKey)))
	if err != nil {
		t.Fatal(err)
	}
	rawKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair([]byte(issued.Certificate), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rawKey}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}}}
	client := &http.Client{Transport: transport}
	defer client.CloseIdleConnections()
	call := func() int {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response.StatusCode
	}
	if call() != http.StatusOK {
		t.Fatal("redeemed certificate did not authenticate MCP")
	}
	if _, err := invite.Redeem(t.Context(), envelope, csr, string(ssh.MarshalAuthorizedKey(sshKey))); err == nil {
		t.Fatal("invite reused over MCP")
	}
	if _, err := fixture.store.RemoveUplink(t.Context(), "owner@example.test", envelope.Identity, issued.SSHFingerprint); err != nil {
		t.Fatal(err)
	}
	if call() != http.StatusUnauthorized {
		t.Fatal("removed invitee retained access")
	}
}
