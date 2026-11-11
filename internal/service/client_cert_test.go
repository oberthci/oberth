package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
)

func TestClientCertificateIssuanceAndRenewalAuthority(t *testing.T) {
	f := newScopeFixture(t)
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	f.service.clientCertificates = &ClientCertIssuer{Authority: ca, Store: f.store}
	u, err := f.store.RegisterCertificateUplink(t.Context(), "operator", model.UplinkSpec{Identity: "cert-agent", Fingerprint: "SHA256:cert-agent"})
	if err != nil {
		t.Fatal(err)
	}
	csr := func() string {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: raw}))
	}
	args := map[string]any{"identity": u.Identity, "csr": csr()}
	for _, actor := range []api.Actor{scopedActor, scopedAdmin, {Identity: "ordinary"}} {
		if _, err := f.call(actor, "uplink_cert", args); !errors.Is(err, ErrForbidden) {
			t.Fatal("unauthorized issuance", err)
		}
	}
	value, err := f.call(unscopedActor, "uplink_cert", args)
	if err != nil {
		t.Fatal(err)
	}
	first := value.(map[string]string)["certificate"]
	block, _ := pem.Decode([]byte(first))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UplinkByClientPin(t.Context(), clientcert.Pin(leaf)); err != nil {
		t.Fatal("returned certificate pin not bound", err)
	}
	actor := api.Actor{Identity: u.Identity, Fingerprint: u.Fingerprint, TokenCredentialID: u.TokenCredentialID, AuthMethod: "bearer"}
	if _, err := f.call(actor, "client_renew", map[string]any{"csr": csr()}); !errors.Is(err, ErrForbidden) {
		t.Fatal("bearer renewed", err)
	}
	actor.AuthMethod = "cert"
	stale := actor
	stale.TokenCredentialID = "replaced-credential"
	if _, err := f.call(stale, "client_renew", map[string]any{"csr": csr()}); !errors.Is(err, ErrForbidden) {
		t.Fatal("replaced credential renewed", err)
	}
	authenticator := &Authenticator{Certificates: f.store}
	authenticated, err := authenticator.AuthenticateCertificate(t.Context(), leaf)
	if err != nil || authenticated.TokenCredentialID != u.TokenCredentialID {
		t.Fatal("certificate authentication lost credential ownership", err)
	}
	if _, err := f.call(actor, "client_renew", map[string]any{"identity": "other", "csr": csr()}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("cross-identity renewal", err)
	}
	value, err = f.call(actor, "client_renew", map[string]any{"csr": csr()})
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode([]byte(value.(map[string]string)["certificate"]))
	next, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if clientcert.Pin(next) == clientcert.Pin(leaf) {
		t.Fatal("renewal did not rotate key")
	}
	for _, cert := range []*x509.Certificate{leaf, next} {
		if _, err := f.store.UplinkByClientPin(t.Context(), clientcert.Pin(cert)); err != nil {
			t.Fatal("overlap lost", err)
		}
	}
	raw, _ := json.Marshal(map[string]any{"csr": csr()})
	f.service.mutationGate = func(_ context.Context) error { return errors.New("audit unavailable") }
	if _, err := f.service.CallTool(t.Context(), actor, "client_renew", raw); err == nil {
		t.Fatal("renewal bypassed audit gate")
	}
}
