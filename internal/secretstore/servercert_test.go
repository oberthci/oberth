package secretstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"slices"
	"strings"
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
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.0.2.193"), net.ParseIP("::1")},
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
	for _, host := range []string{"oberth", "localhost", "127.0.0.1", "192.0.2.193", "::1"} {
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
		{SerialNumber: big.NewInt(3), IPAddresses: []net.IP{net.ParseIP("192.0.2.1")}},
		{SerialNumber: big.NewInt(4), IPAddresses: []net.IP{net.ParseIP("::2")}},
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

func TestConstrainedServerCertificateExactIPRanges(t *testing.T) {
	for _, addresses := range [][]string{nil, {"192.0.2.193"}, {"2001:db8::193"}, {"127.0.0.1", "::1"}} {
		t.Run(strings.Join(addresses, ","), func(t *testing.T) {
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			template := &x509.Certificate{
				SerialNumber: big.NewInt(1), DNSNames: []string{"oberth", "localhost"},
				NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			}
			for _, address := range addresses {
				template.IPAddresses = append(template.IPAddresses, net.ParseIP(address))
			}
			ConstrainSelfSignedServerCertificate(template)
			der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			ca, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			if !ca.PermittedDNSDomainsCritical || !slices.Equal(ca.PermittedDNSDomains, template.DNSNames) || len(ca.PermittedIPRanges) != len(addresses) {
				t.Fatalf("constraints do not match SANs: DNS=%v IP=%v", ca.PermittedDNSDomains, ca.PermittedIPRanges)
			}
			for index, address := range addresses {
				range_ := ca.PermittedIPRanges[index]
				bits := 128
				if net.ParseIP(address).To4() != nil {
					bits = 32
				}
				ones, size := range_.Mask.Size()
				if !range_.IP.Equal(net.ParseIP(address)) || ones != bits || size != bits {
					t.Fatalf("IP constraint %v does not cover exactly %s", range_, address)
				}
			}
			roots := x509.NewCertPool()
			roots.AddCert(ca)
			// Exercise constraints on signed leaves, not just SAN matching on
			// the directly pinned anchor. DNS-only issuance stays unchanged.
			for _, host := range append([]string{"oberth", "outside.example"}, addresses...) {
				leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: template.NotBefore, NotAfter: template.NotAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
				if ip := net.ParseIP(host); ip != nil {
					leaf.IPAddresses = []net.IP{ip}
				} else {
					leaf.DNSNames = []string{host}
				}
				der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := x509.ParseCertificate(der)
				if err != nil {
					t.Fatal(err)
				}
				_, err = parsed.Verify(x509.VerifyOptions{Roots: roots, DNSName: host})
				if (err == nil) != (host != "outside.example") {
					t.Fatalf("leaf %s verification = %v", host, err)
				}
			}
		})
	}
}
