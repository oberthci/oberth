package service_test

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/clientcert"
)

func TestUplinkEnrollmentKindsPersistOneLifetime(t *testing.T) {
	for _, kind := range []string{"ssh", "certificate", "bearer", "certificate-and-bearer"} {
		t.Run(kind, func(t *testing.T) {
			checkUplinkLifetime(t, kind)
		})
	}
}

func checkUplinkLifetime(t *testing.T, kind string) {
	t.Helper()
	p := people(t)
	csr, key := publicKeys(t)
	q := apicontract.UplinkAddArgs{Scope: []string{"*"}, Identity: "agent", SshPublicKey: key, TTL: "2h"}
	if kind == "certificate" || kind == "certificate-and-bearer" {
		q.CSR = csr
	}
	if kind == "bearer" || kind == "certificate-and-bearer" {
		q.BearerDigest = digestText("browser credential")
		if kind == "bearer" {
			q.SshPublicKey = ""
		}
	}
	call, err := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), q)
	issued, err := execute(t.Context(), call, err)
	if err != nil {
		t.Fatal(err)
	}
	want := p.f.c.Now().Add(2 * time.Hour)
	if q.CSR != "" {
		leaf, err := clientcert.ParseCertificate([]byte(issued.Certificate))
		if err != nil {
			t.Fatal(err)
		}
		want = leaf.NotAfter
	}
	uplink, err := p.state.SSHKey(t.Context(), issued.Fingerprint)
	if err != nil || !uplink.ExpiresAt.Equal(want) {
		t.Fatal("enrollment omitted the uplink lifetime", uplink.ExpiresAt, want, err)
	}
}
