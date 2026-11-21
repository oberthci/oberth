package client

import (
	"crypto/tls"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/model"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRenewalReplacesPublicCertificate(t *testing.T) {
	keys, err := generateKeys("agent")
	if err != nil {
		t.Fatal(err)
	}
	ca, key := certificate(t, 100)
	pair, err := tls.X509KeyPair(ca, key)
	if err != nil {
		t.Fatal(err)
	}
	issued, _ := issueEnrollment(t, model.InviteRedemption{CSR: string(keys.csr)}, enrollmentSigner{pair: pair})
	path := privateFile(t, "admin.crt", []byte("old certificate"))
	if err = os.Chmod(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	renewal := Renewal{Key: keys.key, CA: ca, Certificate: issued.Certificate, Now: fake.NewClock().Now()}
	for _, mode := range []os.FileMode{0644, 0444} {
		if err = os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err = InstallRenewed(path, renewal); err != nil {
			t.Fatalf("public certificate refused: %v", err)
		}
		saved, readErr := ReadPublic(path)
		if readErr != nil || string(saved) != issued.Certificate {
			t.Fatal("renewed certificate missing")
		}
	}
	if err = os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	if err = InstallRenewed(path, renewal); err == nil {
		t.Fatal("writable certificate accepted")
	}
}

func TestRenewalRefusesInvalidReplacementWithoutChangingProfile(t *testing.T) {
	keys, err := generateKeys("agent")
	if err != nil {
		t.Fatal(err)
	}
	ca, key := certificate(t, 100)
	pair, err := tls.X509KeyPair(ca, key)
	if err != nil {
		t.Fatal(err)
	}
	issued, _ := issueEnrollment(t, model.InviteRedemption{CSR: string(keys.csr)}, enrollmentSigner{pair: pair})
	wrong, _ := issueEnrollment(t, model.InviteRedemption{CSR: string(keys.csr)}, enrollmentSigner{pair: pair, wrong: true})
	otherCA, _ := certificate(t, 101)
	now := fake.NewClock().Now()
	for _, renewal := range []Renewal{
		{Key: keys.key, CA: ca, Certificate: wrong.Certificate, Now: now},
		{Key: keys.key, CA: otherCA, Certificate: issued.Certificate, Now: now},
		{Key: keys.key, CA: ca, Certificate: issued.Certificate, Now: now.Add(time.Hour)},
		{Key: keys.key, CA: ca, Certificate: "invalid certificate", Now: now},
	} {
		path := privateFile(t, "admin.crt", []byte("original certificate"))
		if err := InstallRenewed(path, renewal); err == nil {
			t.Fatal("invalid renewal installed")
		}
		saved, err := ReadPublic(path)
		if err != nil || string(saved) != "original certificate" {
			t.Fatal("failed renewal changed profile", err)
		}
	}
}
