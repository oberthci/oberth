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
