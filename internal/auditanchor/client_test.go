package auditanchor

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/digitorus/timestamp"

	"github.com/oberthci/oberth/internal/model"
)

var timestampingUsageOID = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 8}

func TestNewClientRejectsFragmentEndpoint(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{
		"https://tsa.example.test/path#fragment",
		"https://tsa.example.test/path#",
	} {
		if _, err := NewClient(ClientConfig{Endpoint: endpoint, Roots: x509.NewCertPool()}); err == nil {
			t.Fatalf("fragment-bearing TSA endpoint passed: %q", endpoint)
		}
	}
}

func TestClientRejectsRedirectToHTTPWithoutMutatingSuppliedClient(t *testing.T) {
	t.Parallel()
	requestAtHTTPSink := make(chan struct{}, 1)
	httpSink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requestAtHTTPSink <- struct{}{}
	}))
	defer httpSink.Close()

	tsa := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, httpSink.URL, http.StatusTemporaryRedirect)
	}))
	defer tsa.Close()
	suppliedClient := tsa.Client()
	if suppliedClient.CheckRedirect != nil {
		t.Fatal("test setup supplied a redirect policy")
	}
	client, err := NewClient(ClientConfig{
		Endpoint: tsa.URL, Roots: x509.NewCertPool(), HTTPClient: suppliedClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("audit head"))
	if _, err := client.Timestamp(context.Background(), model.AuditHead{ID: 42, SHA256: hash[:]}); err == nil {
		t.Fatal("redirected timestamp request succeeded")
	}
	if suppliedClient.CheckRedirect != nil {
		t.Fatal("NewClient mutated the supplied HTTP client")
	}
	select {
	case <-requestAtHTTPSink:
		t.Fatal("HTTP sink received redirected timestamp request")
	default:
	}
}

func TestClientObtainsAndVerifiesRFC3161Checkpoint(t *testing.T) {
	now := time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)
	roots, signingCertificate, signingKey, chain := testTSAIdentity(t, now)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/timestamp-query" {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		query, err := timestamp.ParseRequest(body)
		if err != nil {
			t.Error(err)
			return
		}
		response, err := (&timestamp.Timestamp{
			HashAlgorithm: query.HashAlgorithm, HashedMessage: query.HashedMessage,
			Time: now, Nonce: query.Nonce, Policy: asn1.ObjectIdentifier{1, 2, 3, 4},
			AddTSACertificate: true, Certificates: chain,
		}).CreateResponseWithOpts(signingCertificate, signingKey, crypto.SHA256)
		if err != nil {
			t.Error(err)
			return
		}
		writer.Header().Set("Content-Type", "application/timestamp-reply")
		_, _ = writer.Write(response)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, Roots: roots, HTTPClient: server.Client(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("audit head"))
	spec, err := client.Timestamp(context.Background(), model.AuditHead{ID: 42, SHA256: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	if spec.AuditID != 42 || !bytes.Equal(spec.AuditSHA256, hash[:]) || !spec.AnchoredAt.Equal(now) || len(spec.Receipt) == 0 {
		t.Fatalf("checkpoint = %#v", spec)
	}
	anchor := model.AuditAnchor{
		AuditID: spec.AuditID, AuditSHA256: spec.AuditSHA256, TSAURL: spec.TSAURL,
		Receipt: spec.Receipt, AnchoredAt: spec.AnchoredAt,
	}
	if err := client.Verify(anchor); err != nil {
		t.Fatal(err)
	}
	tampered := anchor
	tampered.AuditSHA256 = append([]byte(nil), anchor.AuditSHA256...)
	tampered.AuditSHA256[0] ^= 0xff
	if err := client.Verify(tampered); err == nil {
		t.Fatal("checkpoint verified against a different audit head")
	}
	untrusted, err := NewClient(ClientConfig{
		Endpoint: server.URL, Roots: x509.NewCertPool(), HTTPClient: server.Client(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Timestamp(context.Background(), model.AuditHead{ID: 42, SHA256: hash[:]}); err == nil {
		t.Fatal("checkpoint from an untrusted TSA verified")
	}
}

func testTSAIdentity(t *testing.T, now time.Time) (*x509.CertPool, *x509.Certificate, *rsa.PrivateKey, []*x509.Certificate) {
	t.Helper()
	// digitorus/pkcs7 adds a signing-time attribute from the real wall clock
	// even when the RFC3161 timestamp payload uses the injected test clock.
	// Cover both instants so this deterministic 2026 fixture cannot expire as
	// the suite's wall-clock execution moves forward.
	wallClock := time.Now().UTC()
	validityStart, validityEnd := now, now
	if wallClock.Before(validityStart) {
		validityStart = wallClock
	}
	if wallClock.After(validityEnd) {
		validityEnd = wallClock
	}
	validityStart = validityStart.Add(-24 * time.Hour)
	validityEnd = validityEnd.Add(24 * time.Hour)
	rootKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Oberth test TSA root"},
		NotBefore: validityStart, NotAfter: validityEnd,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	signingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	usageValue, err := asn1.Marshal([]asn1.ObjectIdentifier{timestampingUsageOID})
	if err != nil {
		t.Fatal(err)
	}
	signingTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Oberth test TSA signer"},
		NotBefore: validityStart, NotAfter: validityEnd,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
		ExtraExtensions: []pkix.Extension{{Id: extendedKeyUsageOID, Critical: true, Value: usageValue}},
	}
	signingDER, err := x509.CreateCertificate(rand.Reader, signingTemplate, root, &signingKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	signing, err := x509.ParseCertificate(signingDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return roots, signing, signingKey, []*x509.Certificate{root}
}
