package secretstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

func TestConstrainedServerCertificateAnchorsOnlyItsOwnNames(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "oberth"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"oberth", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.168.86.193")},
	}
	ConstrainSelfSignedServerCertificate(tmpl)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server)
	for _, host := range []string{"oberth", "localhost", "127.0.0.1", "192.168.86.193"} {
		if _, err := server.Verify(x509.VerifyOptions{DNSName: host, Roots: roots}); err != nil {
			t.Errorf("pinned server certificate must verify for %s: %v", host, err)
		}
	}

	// A certificate the server key signs for any other host must not verify.
	childKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range []*x509.Certificate{
		{SerialNumber: big.NewInt(2), DNSNames: []string{"bank.example"}},
		{SerialNumber: big.NewInt(3), IPAddresses: []net.IP{net.ParseIP("192.168.86.1")}},
	} {
		child.NotBefore, child.NotAfter = time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
		child.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		childDER, err := x509.CreateCertificate(rand.Reader, child, server, &childKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := x509.ParseCertificate(childDER)
		host := "bank.example"
		if len(child.IPAddresses) > 0 {
			host = child.IPAddresses[0].String()
		}
		if _, err := parsed.Verify(x509.VerifyOptions{DNSName: host, Roots: roots}); err == nil {
			t.Errorf("a certificate for %s signed by the server key must be rejected", host)
		}
	}
}
