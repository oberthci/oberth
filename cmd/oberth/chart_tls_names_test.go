package main

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The chart issues the server's certificate for in-cluster names only. Anything
// reaching the server from the host -- the read-only CLI, an MCP client, a
// browser -- presents a name the certificate does not carry, and a name
// mismatch is the one TLS failure no trust anchor can fix. The alternative is
// to tell every operator to edit /etc/hosts, which contradicts what the client
// itself says when it hits this: that the deployment should name the address
// people use.
func TestChartIssuesTheServerCertificateForTheAddressesADeploymentNames(t *testing.T) {
	t.Parallel()
	named := renderServerCertificate(t,
		"--set", "tls.extraDNSNames={localhost,oberth.example.internal}",
		"--set", "tls.extraIPs={127.0.0.1}")

	for _, want := range []string{
		"oberth", "oberth.oberth", "oberth.oberth.svc", "oberth.oberth.svc.cluster.local",
		"localhost", "oberth.example.internal",
	} {
		if !containsString(named.DNSNames, want) {
			t.Errorf("certificate does not carry DNS name %q; it has %v", want, named.DNSNames)
		}
	}
	if len(named.IPAddresses) != 1 || named.IPAddresses[0].String() != "127.0.0.1" {
		t.Errorf("certificate does not carry IP 127.0.0.1; it has %v", named.IPAddresses)
	}
}

// A deployment that names no extra addresses must get exactly what it got
// before. Widening the default would hand every install a certificate valid for
// names it never asked for.
func TestChartAddsNoAddressesADeploymentDidNotName(t *testing.T) {
	t.Parallel()
	bare := renderServerCertificate(t)

	if len(bare.DNSNames) != 4 {
		t.Errorf("default certificate carries %d DNS names, want the 4 in-cluster ones: %v",
			len(bare.DNSNames), bare.DNSNames)
	}
	if containsString(bare.DNSNames, "localhost") {
		t.Error("default certificate carries localhost; a default must not widen on its own")
	}
	if len(bare.IPAddresses) != 0 {
		t.Errorf("default certificate carries IP SANs %v; it should carry none", bare.IPAddresses)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// renderServerCertificate templates the chart and parses the certificate the
// TLS Secret carries, so the assertions are about what a client will actually
// verify rather than about the text of a template.
func renderServerCertificate(t *testing.T, extra ...string) *x509.Certificate {
	t.Helper()
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	args := append([]string{
		"template", "oberth", "../../charts/oberth",
		"--namespace", "oberth",
		"--set", "image.ref=example.invalid/oberth@" + digest,
	}, extra...)

	rendered, err := exec.Command("helm", args...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	// Match the tls.crt from the main TLS secret (type: kubernetes.io/tls),
	// not the goproxy TLS secret (type: Opaque). Split by YAML document
	// separator and find the one with the right type.
	var mainSecretDoc string
	for _, doc := range strings.Split(string(rendered), "\n---\n") {
		if strings.Contains(doc, "type: kubernetes.io/tls") {
			mainSecretDoc = doc
			break
		}
	}
	if mainSecretDoc == "" {
		t.Fatal("rendered chart carries no kubernetes.io/tls Secret")
	}
	match := regexp.MustCompile(`tls\.crt:\s+(\S+)`).FindStringSubmatch(mainSecretDoc)
	if match == nil {
		t.Fatal("kubernetes.io/tls Secret carries no tls.crt")
	}
	pemBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(match[1]))
	if err != nil {
		t.Fatalf("decode tls.crt: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("tls.crt is not PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return certificate
}
