package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
)

type certificateTestAuth struct {
	known bool
	actor Actor
}

func (a *certificateTestAuth) Authenticate(_ context.Context, token string) (Actor, error) {
	if token == "same" {
		return a.actor, nil
	}
	if token == "other" {
		return Actor{Identity: "other", Fingerprint: "other"}, nil
	}
	return Actor{}, ErrInvalidToken
}
func (a *certificateTestAuth) AuthenticateCertificate(_ context.Context, _ *x509.Certificate) (Actor, error) {
	if !a.known {
		return Actor{}, ErrInvalidToken
	}
	return a.actor, nil
}

func TestClientCertificateAuthentication(t *testing.T) {
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
	leafPEM, leaf, err := ca.Issue("agent", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), 0)
	if err != nil {
		t.Fatal(err)
	}
	rawKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(leafPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rawKey}))
	if err != nil {
		t.Fatal(err)
	}
	a := &certificateTestAuth{known: true, actor: Actor{Identity: "agent", Fingerprint: "ssh"}}
	s := &Server{auth: a}
	r := httptest.NewRequest("GET", "https://example.test/api/status", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf, ca.Certificate}}}
	for _, tc := range []struct {
		header string
		want   bool
	}{{"", true}, {"Bearer ", true}, {"Bearer", true}, {"Bearer same", true}, {"Bearer other", false}, {"Bearer invalid", false}, {"Basic bad", false}} {
		r.Header.Del("Authorization")
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		actor, err := s.authenticateRequest(t.Context(), r)
		if (err == nil) != tc.want {
			t.Fatalf("header case: success=%v, want %v", err == nil, tc.want)
		}
		if err == nil && actor.AuthMethod != "cert" {
			t.Fatal("auth method")
		}
	}
	r.Header.Del("Authorization")
	a.known = false
	if _, err := s.authenticateRequest(t.Context(), r); err == nil {
		t.Fatal("removed/unknown pin accepted")
	}
	a.known = true
	leaf.NotAfter = time.Now().Add(-time.Second)
	if _, err := s.authenticateRequest(t.Context(), r); err == nil {
		t.Fatal("expired certificate accepted on reused connection")
	}
	leaf.NotAfter = time.Now().Add(time.Hour)
	r.TLS.VerifiedChains = nil
	if _, err := s.authenticateRequest(t.Context(), r); err == nil {
		t.Fatal("unverified certificate accepted")
	}
	// Exercise the actual TLS listener, including a different CA.
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.authenticateRequest(r.Context(), r); err != nil {
			w.WriteHeader(401)
		}
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	for _, foreign := range []bool{false, true} {
		candidate := pair
		if foreign {
			otherFiles, _ := clientcert.GenerateAuthority()
			other, _ := clientcert.Load(otherFiles["ca.crt"], otherFiles["ca.key"])
			cert, _, err := other.Issue("agent", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), 0)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err = tls.X509KeyPair(cert, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rawKey}))
			if err != nil {
				t.Fatal(err)
			}
		}
		transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &candidate, nil }}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer ")
		resp, err := client.Do(request)
		if foreign {
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("foreign CA accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
	}
}
