package service_test

import (
	"crypto/x509"
	"testing"
	"time"
)

func TestPeopleFixtureUsesInstallationCASignatures(t *testing.T) {
	t.Parallel()
	p := people(t)
	csr, _ := publicKeys(t)
	_, cert, err := p.issuer.Issue("fixture-client", []byte(csr), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if cert.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatal("service fixture does not use installation CA signatures")
	}
}
