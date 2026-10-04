package main

import (
	"crypto/x509"
	"encoding/pem"
	"os/exec"
	"strings"
	"testing"
)

func TestRuntimeIdentityCertificateNames(t *testing.T) {
	data, err := generateRuntimeIdentity("oberth", []string{"localhost", "oberth.example.internal", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data["tls.crt"])
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"localhost", "127.0.0.1", "oberth.example.internal", "oberth.oberth.svc", "oberth-goproxy.oberth.svc"} {
		if err := cert.VerifyHostname(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := cert.VerifyHostname("unrequested.example"); err == nil {
		t.Fatal("unrequested host accepted")
	}
}

func TestChartCarriesPublicNamesAndNoPrivateIdentities(t *testing.T) {
	out, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "-n", "oberth", "--set", "tls.extraDNSNames={localhost,oberth.example.internal}", "--set", "tls.extraIPs={127.0.0.1}").CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v %s", err, out)
	}
	text := string(out)
	for _, forbidden := range []string{"kind: Secret\n", "secretName:", "BEGIN PRIVATE KEY", "BEGIN OPENSSH PRIVATE KEY", "genPrivateKey", "genSelfSignedCert"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("chart contains %s", forbidden)
		}
	}
	for _, want := range []string{"OBERTH_TLS_NAMES", "localhost,oberth.example.internal,127.0.0.1", "OBERTH_IDENTITY_STORE", "medium: Memory"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestIdentityStoreRequiresExplicitDevelopmentHTTP(t *testing.T) {
	t.Setenv("VAULT_ADDR", "http://openbao.openbao.svc:8200")
	t.Setenv("VAULT_CACERT", "")
	t.Setenv("OBERTH_VAULT_ROLE", "oberth-ci")
	t.Setenv("OBERTH_VAULT_AUTH_MOUNT", "custom-kubernetes")
	t.Setenv("OBERTH_VAULT_INSECURE_DEV_HTTP", "false")
	if _, err := newBaoIdentityStore("oberth"); err == nil {
		t.Fatal("plaintext store allowed without explicit development setting")
	}
	t.Setenv("OBERTH_VAULT_INSECURE_DEV_HTTP", "true")
	if _, err := newBaoIdentityStore("oberth"); err != nil {
		t.Fatal(err)
	}
}

func TestChartForwardsIdentityConnectionSettings(t *testing.T) {
	out, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "secretstore.insecureHTTPForDev=true", "--set", "secretstore.k8sAuthMount=custom-kubernetes").CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v %s", err, out)
	}
	for _, pair := range []struct{ name, value string }{
		{"OBERTH_VAULT_INSECURE_DEV_HTTP", "true"},
		{"OBERTH_VAULT_AUTH_MOUNT", "custom-kubernetes"},
		{"VAULT_CACERT", ""},
	} {
		want := "name: " + pair.name + "\n              value: \"" + pair.value + "\""
		if !strings.Contains(string(out), want) {
			t.Errorf("missing identity connection setting %s", pair.name)
		}
	}
}
