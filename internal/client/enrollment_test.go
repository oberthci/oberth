package client

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func issueEnrollment(t *testing.T, input model.InviteRedemption, sign enrollmentSigner) (model.InviteCertificate, *x509.Certificate) {
	t.Helper()
	wrong, pair := sign.wrong, sign.pair
	now := fake.NewClock().Now()
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	csr, err := clientcert.ParseCSR([]byte(input.CSR))
	if err != nil {
		t.Fatal(err)
	}
	public := csr.PublicKey
	if wrong {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		public = &key.PublicKey
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: csr.Subject, NotBefore: now, NotAfter: now.Add(30 * time.Minute), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, public, pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return model.InviteCertificate{SSHURL: "ssh://git@oberth.local:2222", Identity: "agent", Scope: []string{"project"}, Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}, ca
}

type enrollmentSigner struct {
	wrong bool
	pair  tls.Certificate
}
type enrollmentFixture struct {
	config     Config
	invitation model.InvitationFile
	calls      *int
	authority  []byte
}

func newEnrollment(t *testing.T, wrong bool, dir string) enrollmentFixture {
	t.Helper()
	if err := os.Chmod(filepath.Dir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	clock := fake.NewClock()
	calls := 0
	handler := &fake.Handler[model.InviteRedemption, model.InviteCertificate]{}
	cert, key := certificate(t, 98)
	fixed, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	var ca *x509.Certificate
	handler.Run = func(context.Context) (model.InviteCertificate, error) {
		calls++
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			return model.InviteCertificate{}, publicerr.New(publicerr.Invariant, "An unfinished join profile is visible.", "Publish the complete profile after verification.", nil)
		}
		stages, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), ".join-*"))
		if len(stages) != 1 {
			return model.InviteCertificate{}, publicerr.New(publicerr.Unavailable, "Join staging is missing.", "Prepare the private profile before redeeming.", nil)
		}
		for _, name := range []string{"admin.key", "admin.csr", UplinkPushKeyFile, "push.pub", HostKeysFile, "admin.crt", "ca.crt", "installation.json"} {
			info, err := os.Stat(filepath.Join(stages[0], name))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Errorf("redeemed before every file was reserved: %s", name)
				return model.InviteCertificate{}, publicerr.New(publicerr.Unavailable, "Join file was not reserved.", "Prepare every profile file before redeeming.", nil)
			}
		}
		inputs, _, _ := handler.Observed()
		result, root := issueEnrollment(t, inputs[len(inputs)-1], enrollmentSigner{wrong, fixed})
		ca = root
		return result, nil
	}
	h := server(t, fake.Bindings(), handler)
	// This test CA is deterministic for the transport fixture's pin binding.
	parsed, err := clientcert.ParseCertificate(cert)
	if err != nil {
		t.Fatal(err)
	}
	invitation := model.InvitationFile{Invitation: model.Invitation{Identity: "agent", Scope: []string{"project"}, ServerURL: "https://oberth.local", CAPin: clientcert.Pin(parsed), ExpiresAt: clock.Now().Add(time.Hour)}, KnownHosts: fixtureHostKeys, Secret: "private-handoff"}
	transport := roundTrip(func(r *http.Request) (*http.Response, error) {
		response, err := handlerTransport(h).RoundTrip(r)
		if ca != nil {
			response.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{ca, ca}}
		}
		return response, err
	})
	// The response uses an actual native CA; set its expected pin before the request.

	return enrollmentFixture{Config{Transport: transport, Now: clock.Now, Wait: func(context.Context, time.Duration) error { t.Fatal("enrollment replay"); return nil }}, invitation, &calls, cert}
}

func TestRedeemPreparesThenVerifies(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "wrong key"}[wrong], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "profile")
			f := newEnrollment(t, wrong, dir)
			_, err := Join(context.Background(), f.config, f.invitation, dir)
			if *f.calls != 1 {
				t.Fatalf("redemption count %d: %v", *f.calls, err)
			}
			if wrong {
				if err == nil || !strings.Contains(err.Error(), "invitation status is unknown") {
					t.Fatal("wrong key not refused after consumption", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOccupiedProfileRefusesBeforeRedeem(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "profile")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f := newEnrollment(t, false, dir)
	if _, err := Join(context.Background(), f.config, f.invitation, dir); err == nil || *f.calls != 0 {
		t.Fatal("occupied path redeemed invitation")
	}
}

// Covers: REQ-C11-012
func TestOneTimeBearerActivationContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	called := 0
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	err := ActivateBearer(context.Background(), path, func(_ context.Context, digest string) error {
		called++
		saved, err := ReadPrivate(path)
		if err != nil {
			t.Fatal(err)
		}
		if digest != apicontract.SecretDigest(string(saved)) || digest == string(saved) || len(digest) != 64 {
			t.Fatal("activation did not receive only the saved digest")
		}
		return nil
	})
	if err != nil || called != 1 {
		t.Fatal("activation order", err)
	}
	if err = ActivateBearer(context.Background(), path, func(context.Context, string) error { called++; return nil }); err == nil || called != 1 {
		t.Fatal("occupied file activated bearer")
	}
}

func TestInvitePrivateHandoffBeforeRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invite.json")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	b := fake.Bindings()
	h := &fake.Handler[apicontract.UplinkInviteArgs, model.Invitation]{}
	h.Run = func(context.Context) (model.Invitation, error) {
		data, err := ReadPrivate(path)
		if err != nil {
			t.Fatal(err)
		}
		var file model.InvitationFile
		if json.Unmarshal(data, &file) != nil {
			t.Fatal("invalid handoff")
		}
		inputs, _, _ := h.Observed()
		if apicontract.SecretDigest(file.Secret) != inputs[0].SecretDigest {
			return model.Invitation{}, publicerr.New(publicerr.InvalidInput, "Digest does not match the saved handoff.", "Send only the stored invitation digest.", nil)
		}
		return model.Invitation{Identity: "agent", ServerURL: "https://oberth.local", ExpiresAt: fake.NewClock().Now().Add(time.Hour)}, nil
	}
	b.UplinkInvite.Handler = h
	c := testClient(t, handlerTransport(server(t, b)), nil)
	if _, err := c.Invite(context.Background(), apicontract.UplinkInviteArgs{Identity: "agent", ServerURL: "https://oberth.local"}, path, fixtureHostKeys); err != nil {
		t.Fatal(err)
	}
}

func TestProfileSelectionRefusesBeforeRedemption(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "profile")
	f := newEnrollment(t, false, dir)
	if err := os.WriteFile(filepath.Join(base, "current"), []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Join(context.Background(), f.config, f.invitation, dir); err == nil || *f.calls != 0 {
		t.Fatal("selection path was not prepared before redemption")
	}
}

func TestInvitationPeerPinAndName(t *testing.T) {
	now := fake.NewClock().Now()
	pemCA, key := certificate(t, 44)
	pair, err := tls.X509KeyPair(pemCA, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(45), DNSNames: []string{"oberth.local"}, NotBefore: now, NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, pair.PrivateKey.(interface{ Public() crypto.PublicKey }).Public(), pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	served, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{served, ca}}
	invite := model.InvitationFile{Invitation: model.Invitation{ServerURL: "https://oberth.local", CAPin: clientcert.Pin(ca)}}
	if err = verifyInvitationPeer(state, invite, now); err != nil {
		t.Fatal(err)
	}
	invite.ServerURL = "https://other.local"
	if verifyInvitationPeer(state, invite, now) == nil {
		t.Fatal("hostname not checked")
	}
	invite.ServerURL = "https://oberth.local"
	invite.CAPin = strings.Repeat("0", 64)
	if verifyInvitationPeer(state, invite, now) == nil {
		t.Fatal("pin not checked")
	}
}

func TestEnrollmentRejectsWrongUsageCAAndValidity(t *testing.T) {
	now := fake.NewClock().Now()
	caPEM, keyPEM := certificate(t, 55)
	pair, err := tls.X509KeyPair(caPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	keys, err := generateKeys("agent")
	if err != nil {
		t.Fatal(err)
	}
	invitation := model.InvitationFile{Invitation: model.Invitation{Identity: "agent", ServerURL: "https://oberth.local", Scope: []string{"project"}, CAPin: clientcert.Pin(ca)}}
	for _, mode := range []string{"client", "server", "ca", "expired", "future"} {
		template := &x509.Certificate{SerialNumber: big.NewInt(56), Subject: pkix.Name{CommonName: "agent"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
		switch mode {
		case "server":
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		case "ca":
			template.IsCA = true
		case "expired":
			template.NotAfter = now
		case "future":
			template.NotBefore = now.Add(time.Second)
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, keys.public, pair.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		result := model.InviteCertificate{SSHURL: "ssh://git@oberth.local:2222", Identity: "agent", Scope: []string{"project"}, Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
		err = verifyEnrolled(result, invitation, enrolledMaterial{keys, caPEM, now})
		if (err == nil) != (mode == "client") {
			t.Fatalf("%s validity accepted=%v", mode, err == nil)
		}
	}
}
