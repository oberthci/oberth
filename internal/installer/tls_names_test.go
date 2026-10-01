package installer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// secretWithCertificate builds the Secret the chart would have written, with a
// real certificate carrying the given names, so the assertions are about what a
// client verifies rather than about a fixture string.
func secretWithCertificate(t *testing.T, dnsNames []string, ips []string) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "oberth"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsNames,
	}
	for _, address := range ips {
		template.IPAddresses = append(template.IPAddresses, net.ParseIP(address))
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "oberth-tls", Namespace: "oberth"},
		Data:       map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})},
	}
}

// A deployment reached on an address its certificate does not carry fails TLS
// with a hostname mismatch, and no trust anchor repairs that. The installer is
// the only component that knows both which addresses this install will be
// reached on and that the certificate is about to be generated, so it is the
// place that names them.
func TestOberthHelmArgsNamesTheAddressesTheCertificateMustCarry(t *testing.T) {
	t.Parallel()

	args := strings.Join(OberthHelmArgs(Config{
		TLSExtraDNSNames: []string{"localhost", "oberth.example.internal"},
		TLSExtraIPs:      []string{"127.0.0.1"},
	}, OpenBaoResult{}, RekorResult{}), " ")

	for _, want := range []string{
		"tls.extraDNSNames[0]=localhost",
		"tls.extraDNSNames[1]=oberth.example.internal",
		"tls.extraIPs[0]=127.0.0.1",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("helm args do not set %q; got %s", want, args)
		}
	}
}

// An install that names no extra addresses must send no extra values, so the
// chart default stays the four in-cluster names.
func TestOberthHelmArgsSendsNoCertificateNamesWhenNoneWereGiven(t *testing.T) {
	t.Parallel()

	args := strings.Join(OberthHelmArgs(Config{}, OpenBaoResult{}, RekorResult{}), " ")
	if strings.Contains(args, "tls.extra") {
		t.Errorf("helm args widen the certificate for an install that named nothing: %s", args)
	}
}

// A kind install is reached over the chart's fixed NodePorts on the loopback
// interface, so localhost and 127.0.0.1 are always true of it. Adding them
// automatically is what removes the /etc/hosts step a host-side client would
// otherwise need.
func TestKindInstallNamesLoopbackOnTheCertificate(t *testing.T) {
	t.Parallel()

	dnsNames, ips := certificateNamesForKind(nil, nil)
	if !contains(dnsNames, "localhost") {
		t.Errorf("a kind install does not name localhost: %v", dnsNames)
	}
	if !contains(ips, "127.0.0.1") {
		t.Errorf("a kind install does not name 127.0.0.1: %v", ips)
	}
}

// An operator who names their own addresses keeps them, and does not lose the
// loopback names that are true regardless.
func TestKindInstallKeepsOperatorNamesAndDoesNotDuplicateLoopback(t *testing.T) {
	t.Parallel()

	dnsNames, ips := certificateNamesForKind([]string{"oberth.example.internal", "localhost"}, []string{"127.0.0.1"})

	if !contains(dnsNames, "oberth.example.internal") {
		t.Errorf("operator-named address was dropped: %v", dnsNames)
	}
	if countOf(dnsNames, "localhost") != 1 {
		t.Errorf("localhost appears %d times, want exactly 1: %v", countOf(dnsNames, "localhost"), dnsNames)
	}
	if countOf(ips, "127.0.0.1") != 1 {
		t.Errorf("127.0.0.1 appears %d times, want exactly 1: %v", countOf(ips, "127.0.0.1"), ips)
	}
}

func contains(values []string, want string) bool { return countOf(values, want) > 0 }

func countOf(values []string, want string) int {
	total := 0
	for _, value := range values {
		if value == want {
			total++
		}
	}
	return total
}

// Naming an address on an existing release does nothing on its own: the TLS
// Secret is generated once and kept, and the template renders nothing when it
// is already there. Emitting helm values that will not take effect, and saying
// nothing, is how an operator concludes the flag is broken. The installer holds
// the Secret and can simply look.
func TestExistingCertificateMissingNamesIsReported(t *testing.T) {
	t.Parallel()

	deps := Deps{KubeClient: fake.NewSimpleClientset(secretWithCertificate(t, []string{"oberth"}, nil))}
	missing := certificateNamesNotYetIssued(context.Background(), Config{
		TLSExtraDNSNames: []string{"localhost", "oberth.example.internal"},
		TLSExtraIPs:      []string{"127.0.0.1"},
	}, deps)

	for _, want := range []string{"localhost", "oberth.example.internal", "127.0.0.1"} {
		if !contains(missing, want) {
			t.Errorf("%q is not covered by the existing certificate but was not reported: %v", want, missing)
		}
	}
}

// A certificate that already carries the names must produce no warning, or the
// warning fires on every re-run of a kind install and stops being read.
func TestExistingCertificateThatAlreadyCoversTheNamesIsSilent(t *testing.T) {
	t.Parallel()

	deps := Deps{KubeClient: fake.NewSimpleClientset(
		secretWithCertificate(t, []string{"oberth", "localhost"}, []string{"127.0.0.1"}))}
	missing := certificateNamesNotYetIssued(context.Background(), Config{
		TLSExtraDNSNames: []string{"localhost"},
		TLSExtraIPs:      []string{"127.0.0.1"},
	}, deps)

	if len(missing) != 0 {
		t.Errorf("a certificate that already covers the names produced a warning: %v", missing)
	}
}

// A fresh install has no Secret yet and is about to get exactly what it asked
// for, so there is nothing to warn about.
func TestFreshInstallReportsNothingMissing(t *testing.T) {
	t.Parallel()

	deps := Deps{KubeClient: fake.NewSimpleClientset()}
	missing := certificateNamesNotYetIssued(context.Background(), Config{
		TLSExtraDNSNames: []string{"localhost"},
	}, deps)

	if len(missing) != 0 {
		t.Errorf("a fresh install warned about a certificate that does not exist yet: %v", missing)
	}
}
