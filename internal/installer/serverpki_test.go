package installer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// newTestServerCA stands in for the CA OpenBao generates: self-signed, CA,
// pathlen 0, critical DNS name constraints.
func newTestServerCA(dnsNames []string) []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Oberth server CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(serverCAValidity),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
		PermittedDNSDomainsCritical: true, PermittedDNSDomains: dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestServerCACoversItsDNSNames(t *testing.T) {
	t.Parallel()
	ca, err := parseCertificatePEM(newTestServerCA([]string{"oberth", "localhost"}))
	if err != nil {
		t.Fatal(err)
	}
	if !serverCACovers(ca, []string{"oberth", "localhost"}) {
		t.Fatal("CA must cover the names it was issued for")
	}
	if serverCACovers(ca, []string{"oberth", "tuxbox"}) {
		t.Fatal("a new DNS name must require a new CA")
	}
	ca.PermittedIPRanges = []*net.IPNet{{IP: net.ParseIP("127.0.0.1").To4(), Mask: net.CIDRMask(32, 32)}}
	if serverCACovers(ca, []string{"oberth"}) {
		t.Fatal("a CA with IP constraints is unusable for BoringSSL clients and must be replaced")
	}
}

// OpenBao returns TTLs as seconds and lists in its own order; a role that
// matches must not be rewritten on every install.
func TestServerPKIRoleMatchesOpenBaoShapedRole(t *testing.T) {
	t.Parallel()
	want := serverPKIRole([]string{"oberth", "localhost"}, []string{"127.0.0.1/32"})
	stored := map[string]any{}
	raw, _ := json.Marshal(want)
	_ = json.Unmarshal(raw, &stored)
	stored["ttl"], stored["max_ttl"] = float64(2592000), float64(2592000)
	stored["allowed_domains"] = []any{"localhost", "oberth"}
	if !serverPKIRoleMatches(stored, want) {
		t.Fatal("an equivalent stored role must match")
	}
	stored["allowed_domains"] = []any{"oberth"}
	if serverPKIRoleMatches(stored, want) {
		t.Fatal("a role missing a name must not match")
	}
}

// First provisioning has OpenBao generate the CA (no key ever reaches the
// installer), makes it the default issuer, writes the issuing role and lets
// the server policy issue its own leaf.
func TestConfigureSecretStoreProvisionsServerPKI(t *testing.T) {
	t.Parallel()
	runner := &fakeBaoRunner{t: t, responses: freshStoreResponses()}
	deps := secretStoreTestDeps(runner, io.Discard)
	cfg := Config{InstallSecretStore: true, TLSExtraIPs: []string{"127.0.0.1"}, TLSExtraDNSNames: []string{"localhost"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store := newOpenBaoExec(deps, DefaultOpenBaoNamespace, "openbao-0")
	result, err := ConfigureSecretStore(context.Background(), cfg, deps, store, "root-token-x")
	if err != nil {
		t.Fatal(err)
	}
	if result.ServerCACertPEM == "" || !result.ServerCAReplaced {
		t.Fatalf("server CA not provisioned: %+v", result)
	}
	calls := runner.callsByCommand()
	generated, ok := calls["write -format=json oberth-pki/root/generate/internal -"]
	if !ok || !strings.Contains(generated.stdin, `"permitted_dns_domains":[`) || !strings.Contains(generated.stdin, `"localhost"`) {
		t.Fatalf("CA must be generated inside OpenBao, constrained to the server names: %q", generated.stdin)
	}
	for _, call := range runner.calls {
		if strings.Contains(call.stdin, "PRIVATE KEY") || strings.Contains(strings.Join(call.argv, " "), "PRIVATE KEY") {
			t.Fatalf("no private key may pass through the installer: %s", call.command)
		}
	}
	role, ok := calls["write oberth-pki/roles/oberth-server -"]
	if !ok || !strings.Contains(role.stdin, `"localhost"`) || !strings.Contains(role.stdin, `"127.0.0.1/32"`) {
		t.Fatalf("role must allow exactly the server names and IPs: %q", role.stdin)
	}
	if _, ok := calls["write oberth-pki/config/issuers -"]; !ok {
		t.Fatal("the generated CA must become the default issuer")
	}
	policy := calls["policy write oberth-ci -"]
	if !strings.Contains(policy.stdin, `path "oberth-pki/issue/oberth-server"`) {
		t.Fatal("the server policy must allow issuing its own leaf")
	}
}
