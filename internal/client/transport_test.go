package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
)

func installationBoundTransportContract(t *testing.T) {
	for _, address := range []string{"http://127.0.0.1", "http://[::1]", "http://localhost", "http://oberth.local", "http://192.0.2.1", "https://user@oberth.local", "https://oberth.local?x=1", "file:///a", "https://oberth.local#fragment"} {
		if validateAddress(address) == nil {
			t.Fatalf("unsafe address accepted: %s", address)
		}
	}
	for _, address := range []string{"https://oberth.local"} {
		if err := validateAddress(address); err != nil {
			t.Fatal(err)
		}
	}
	requests := 0
	c := testClient(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests > 1 {
			return nil, errors.New("redirect limit reached")
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://other.local/mcp"}}, Body: http.NoBody, Request: r}, nil
	}), nil)
	_, _, err := c.Call(context.Background(), definition(t, "repo_list"), json.RawMessage(`{}`))
	if err == nil || requests != 1 {
		t.Fatal("redirect followed")
	}
}

func certificate(t *testing.T, serial int64, names ...string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := fake.NewClock().Now()
	cert := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "client"}, NotBefore: now, NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	cert.DNSNames = names
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}

func TestPinnedTrustAndCertificateReload(t *testing.T) {
	cert, key := certificate(t, 1)
	certPath := privateFile(t, "admin.crt", cert)
	keyPath := privateFile(t, "admin.key", key)
	rt, err := transport(Options{CAFile: privateFile(t, "ca.crt", cert), ClientCert: certPath, ClientKey: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.CloseIdleConnections()
	if rt.Proxy != nil || rt.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("unpinned transport")
	}
	first, err := rt.TLSClientConfig.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	assertPin(t, rt, first)
	secondCert, secondKey := certificate(t, 2)
	if err = WritePrivate(certPath, secondCert); err != nil {
		t.Fatal(err)
	}
	if err = WritePrivate(keyPath, secondKey); err != nil {
		t.Fatal(err)
	}
	second, err := rt.TLSClientConfig.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil || string(first.Certificate[0]) == string(second.Certificate[0]) {
		t.Fatal("stale certificate")
	}
	if err = os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = rt.TLSClientConfig.GetClientCertificate(&tls.CertificateRequestInfo{}); err == nil {
		t.Fatal("unsafe rotated key accepted")
	}
	if _, err = transport(Options{CAFile: privateFile(t, "bad-ca", []byte("bad"))}); err == nil {
		t.Fatal("bad CA accepted")
	}
}

func assertPin(t *testing.T, rt *http.Transport, first *tls.Certificate) {
	t.Helper()
	parsed, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = parsed.Verify(x509.VerifyOptions{Roots: rt.TLSClientConfig.RootCAs, CurrentTime: fake.NewClock().Now()}); err != nil {
		t.Fatal("CA pin rejected")
	}
}
