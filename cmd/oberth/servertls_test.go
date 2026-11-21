package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
	"golang.org/x/crypto/ssh"
)

func authority(t *testing.T, now time.Time) ([]byte, []byte) {
	t.Helper()
	return authorityLifetime(t, now, 7*24*time.Hour)
}
func authorityLifetime(t *testing.T, now time.Time, lifetime time.Duration) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(lifetime)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: secret})
}

// Covers: REQ-C11-027
func TestServedLeafRenews(t *testing.T) {
	clock := fake.NewClock()
	cert, key := authority(t, clock.Now())
	identity, err := newServerIdentity(cert, key, []string{"oberth.local"}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := identity.certificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Certificate) != 2 {
		t.Fatal("CA missing from served chain")
	}
	roots := x509.NewCertPool()
	roots.AddCert(identity.ca.Leaf)
	if _, err = first.Leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: clock.Now(), DNSName: "oberth.local"}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(16*time.Hour - time.Nanosecond)
	before, err := identity.certificate(nil)
	if err != nil || before != first {
		t.Fatal("renewed before threshold")
	}
	clock.Advance(time.Nanosecond)
	renewed, err := identity.certificate(nil)
	if err != nil || renewed == first {
		t.Fatal("did not renew at two thirds")
	}
	clock.Advance(8 * 24 * time.Hour)
	if identity.valid() {
		t.Fatal("expired leaf is valid")
	}
	if _, err = identity.certificate(nil); err == nil {
		t.Fatal("expired CA issued a leaf")
	}
}

func TestAdminKeyFromSecret(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	var last []byte
	install := func(_ context.Context, admin, push []byte) error {
		calls++
		last = append([]byte(nil), admin...)
		return nil
	}
	if installTestAdmin(context.Background(), dir, install) == nil || calls != 0 {
		t.Fatal("missing admin Secret accepted")
	}
	first := writeAdminKey(t, dir, "admin.pub")
	writeAdminKey(t, dir, "push.pub")
	if err := installTestAdmin(context.Background(), dir, install); err != nil || calls != 1 || string(last) != string(first) {
		t.Fatal("admin key not delivered")
	}
	second := writeAdminKey(t, dir, "admin.pub")
	if err := installTestAdmin(context.Background(), dir, install); err != nil || calls != 2 || string(last) != string(second) {
		t.Fatal("replacement key not delivered")
	}
	if err := os.WriteFile(filepath.Join(dir, "admin.pub"), []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	if installTestAdmin(context.Background(), dir, install) == nil || calls != 2 {
		t.Fatal("malformed key delivered")
	}
}

func TestServerMessageCatalogue(t *testing.T) {
	for key, m := range serverMessages() {
		if publicerr.CheckMessage(m[0], 80) != nil || publicerr.CheckMessage(m[1], 120) != nil {
			t.Fatal("invalid server message", key)
		}
	}
}

func writeAdminKey(t *testing.T, dir, name string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	data := ssh.MarshalAuthorizedKey(public)
	if err = os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func installTestAdmin(ctx context.Context, dir string, install func(context.Context, []byte, []byte) error) error {
	keys, err := readAdmin(filepath.Join(dir, "admin.pub"), filepath.Join(dir, "push.pub"))
	if err != nil {
		return err
	}
	return keys.install(ctx, install)
}
