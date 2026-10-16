package secretstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

func testIssuedLeaf(t *testing.T, dnsNames []string, ips []string) IssuedServerCertificate {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour * 24),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: dnsNames[0]},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(30 * 24 * time.Hour),
		DNSNames: dnsNames, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, ip := range ips {
		leafTemplate.IPAddresses = append(leafTemplate.IPAddresses, net.ParseIP(ip))
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(leafKey)
	return IssuedServerCertificate{
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		Key:         pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		CA:          pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
	}
}

func TestIssuedServerCertificateValidation(t *testing.T) {
	t.Parallel()
	request := ServerCertificateRequest{DNSNames: []string{"oberth", "localhost"}, IPs: []string{"127.0.0.1"}}
	issued := testIssuedLeaf(t, request.DNSNames, request.IPs)
	if err := issued.validate(request); err != nil {
		t.Fatalf("valid leaf rejected: %v", err)
	}
	if renew := issued.RenewAt(); !renew.After(issued.NotBefore) || !renew.Before(issued.NotAfter) {
		t.Fatalf("renewal %s must fall inside the leaf lifetime", renew)
	}
	// A leaf that misses a requested name is refused.
	partial := testIssuedLeaf(t, []string{"oberth"}, nil)
	if err := partial.validate(request); err == nil || !strings.Contains(err.Error(), "localhost") {
		t.Fatalf("leaf without localhost accepted: %v", err)
	}
}

func TestServerTLSNamesDeduplicatesAndSplitsIPs(t *testing.T) {
	t.Parallel()
	dns, ips := ServerTLSNames("oberth", []string{"tuxbox", "", "127.0.0.1", "tuxbox", "oberth"})
	if !slices.Contains(dns, "tuxbox") || slices.Contains(dns, "127.0.0.1") || !slices.Equal(ips, []string{"127.0.0.1"}) {
		t.Fatalf("dns=%v ips=%v", dns, ips)
	}
	seen := map[string]bool{}
	for _, name := range dns {
		if seen[name] {
			t.Fatalf("duplicate %s in %v", name, dns)
		}
		seen[name] = true
	}
}
