package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
)

func TestRenewSelectedProfileRetainsKeyAndInstallsCertificate(t *testing.T) {
	now := fake.NewClock().Now()
	ca, key := authority(t, now)
	pair, err := tls.X509KeyPair(ca, key)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, csr := renewalKey(t)

	cert := renewalCertificate(t, pair, csr, now)
	b := fake.Bindings()
	h := &fake.Handler[apicontract.ClientRenewArgs, apicontract.Identity]{}
	h.Run = func(context.Context) (apicontract.Identity, error) {
		requests, _, _ := h.Observed()
		return apicontract.Identity{Certificate: renewalCertificate(t, pair, []byte(requests[len(requests)-1].CSR), now.Add(time.Minute))}, nil
	}
	b.ClientRenew.Handler = h
	a, out, diagnostic := fixture(t, b)
	dir := renewalProfile(t, map[string][]byte{"admin.key": clientKey, "admin.crt": []byte(cert), "ca.crt": ca})
	args := []string{"renew", "--client-key", filepath.Join(dir, "admin.key"), "--client-cert", filepath.Join(dir, "admin.crt"), "--ca-file", filepath.Join(dir, "ca.crt")}
	if a.run(t.Context(), args) != 0 || diagnostic.Len() != 0 {
		t.Fatal("renew failed", diagnostic.String())
	}

	inputs, _, calls := h.Observed()
	if calls != 1 || !strings.Contains(inputs[0].CSR, "BEGIN CERTIFICATE REQUEST") {
		t.Fatal("renew did not submit CSR")
	}
	saved, err := client.ReadPublic(filepath.Join(dir, "admin.crt"))
	if err != nil || bytes.Equal(saved, []byte(cert)) || out.String() != "certificate renewed\nnext:\noberth system status\n" {
		t.Fatal("renew did not install or report replacement", err, out.String())
	}
	retained, err := client.ReadPrivate(filepath.Join(dir, "admin.key"))
	if err != nil || !bytes.Equal(retained, clientKey) {
		t.Fatal("renew changed retained key", err)
	}
}
func renewalCertificate(t *testing.T, pair tls.Certificate, raw []byte, now time.Time) string {
	t.Helper()
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("renewal CSR missing")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		t.Fatal("renewal CSR invalid", err)
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, csr.PublicKey, pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func renewalProfile(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := client.WritePrivate(filepath.Join(dir, name), data); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func renewalKey(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	csr, err := client.RenewalCSR(private)
	if err != nil {
		t.Fatal(err)
	}
	return private, csr
}
