package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/invite"
)

func TestUplinkInviteCSRRedemption(t *testing.T) {
	f := newScopeFixture(t)
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	pin := strings.Repeat("a", 64)
	f.service.clientCertificates = &ClientCertIssuer{Authority: ca, Store: f.store, ServerCAPin: pin}
	secret, digest, err := invite.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	arguments := map[string]any{"identity": "agent@example.test", "server_url": "https://oberth.example.test/mcp", "secret_digest": digest, "scope": []string{"github"}, "expires": "1h"}
	for _, actor := range []api.Actor{scopedActor, scopedAdmin, {Identity: "ordinary@example.test"}} {
		if _, err := f.call(actor, "uplink_invite", arguments); !errors.Is(err, ErrForbidden) {
			t.Fatal("unauthorized invite creation", err)
		}
	}
	value, err := f.call(unscopedActor, "uplink_invite", arguments)
	if err != nil {
		t.Fatal(err)
	}
	binding := value.(invite.Binding)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), digest) {
		t.Fatal("invite creation exposed a credential")
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	args := InviteRedemption{Secret: secret, ServerURL: binding.ServerURL, CAPin: binding.CAPin, CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})), SSHPublicKey: string(ssh.MarshalAuthorizedKey(sshKey))}
	call := func(args InviteRedemption) (any, error) {
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		return f.service.RedeemInvite(t.Context(), raw)
	}
	unknown := args
	unknown.Secret, _, err = invite.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	unknown.CSR = "invalid"
	if _, err := call(unknown); !errors.Is(err, ErrForbidden) {
		t.Fatal("unauthenticated invite reached CSR verification", err)
	}
	invalid := args
	invalid.CSR = "invalid"
	if _, err := call(invalid); err == nil {
		t.Fatal("invalid CSR accepted")
	}
	invalid = args
	invalid.CAPin = strings.Repeat("b", 64)
	if _, err := call(invalid); err == nil {
		t.Fatal("wrong CA pin accepted")
	}
	invalid = args
	invalid.SSHPublicKey = "invalid"
	if _, err := call(invalid); err == nil {
		t.Fatal("invalid SSH key accepted")
	}
	previousGate := f.service.mutationGate
	f.service.mutationGate = func(context.Context) error { return errors.New("audit unavailable") }
	if _, err := call(args); err == nil {
		t.Fatal("redemption bypassed audit mutation gate")
	}
	f.service.mutationGate = previousGate
	value, err = call(args)
	if err != nil {
		t.Fatal(err)
	}
	issued := value.(InviteCertificate)
	block, _ := pem.Decode([]byte(issued.Certificate))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.CheckSignatureFrom(ca.Certificate); err != nil {
		t.Fatal(err)
	}
	u, err := f.store.UplinkByClientPin(t.Context(), clientcert.Pin(leaf))
	if err != nil {
		t.Fatal(err)
	}
	if u.Identity != binding.Identity || u.Fingerprint != ssh.FingerprintSHA256(sshKey) || len(u.Scope) != 1 || u.Scope[0] != "github" || u.Admin {
		t.Fatal("invite authority or SSH key changed")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "token") {
		t.Fatal("redemption returned a secret")
	}
	if _, err := call(args); !errors.Is(err, ErrForbidden) {
		t.Fatal("invite reused", err)
	}
}
