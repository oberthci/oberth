package goproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// selfSignedCA returns a CA cert/key and a leaf cert/key signed by the CA.
func selfSignedCA(t *testing.T) (caCert, caKey, leafCert, leafKey []byte) {
	t.Helper()
	caPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-goproxy-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caPriv.PublicKey, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	caCert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caKeyDER, _ := x509.MarshalECPrivateKey(caPriv)
	caKey = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER})

	leafPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	parsedCA, _ := x509.ParseCertificate(caDER)
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, parsedCA, &leafPriv.PublicKey, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	leafCert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	leafKeyDER, _ := x509.MarshalECPrivateKey(leafPriv)
	leafKey = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER})
	return
}

func TestHandlerOverTLS(t *testing.T) {
	caCert, _, leafCert, leafKey := selfSignedCA(t)

	cert, err := tls.X509KeyPair(leafCert, leafKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := testHandler()
	server := &http.Server{
		Handler: handler,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
		},
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", server.TLSConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		t.Fatal("failed to add CA cert")
	}
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS13,
				RootCAs:    caPool,
			},
		},
	}

	resp, err := client.Get("https://" + listener.Addr().String() + "/go.example.test/wire/@v/list")
	if err != nil {
		t.Fatalf("GET over TLS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS13 {
		t.Fatal("connection did not negotiate TLS 1.3")
	}
}

func TestTLS12Refused(t *testing.T) {
	_, _, leafCert, leafKey := selfSignedCA(t)

	cert, err := tls.X509KeyPair(leafCert, leafKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := testHandler()
	server := &http.Server{
		Handler: handler,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
		},
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", server.TLSConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				MaxVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true,
			},
		},
	}
	_, err = client.Get("https://" + listener.Addr().String() + "/go.example.test/wire/@v/list")
	if err == nil {
		t.Fatal("expected TLS 1.2 to be refused")
	}
	if !strings.Contains(err.Error(), "protocol version") && !strings.Contains(err.Error(), "tls") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPOSTRefusedOverTLS(t *testing.T) {
	caCert, _, leafCert, leafKey := selfSignedCA(t)

	cert, err := tls.X509KeyPair(leafCert, leafKey)
	if err != nil {
		t.Fatal(err)
	}
	handler := testHandler()
	server := &http.Server{
		Handler: handler,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
		},
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", server.TLSConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS13,
				RootCAs:    caPool,
			},
		},
	}

	resp, err := client.Post("https://"+listener.Addr().String()+"/go.example.test/wire/@v/list", "", nil)
	if err != nil {
		t.Fatalf("POST over TLS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}
