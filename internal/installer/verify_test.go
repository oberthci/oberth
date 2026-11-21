package installer_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sync"
	"sync/atomic"
)

// Covers: REQ-C9-009
func TestVerifiedInstallationTlsContract(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := apply(t, f, prepare(t, f))
	r.Profile.ServerURL = "https://server.oberth.ci"
	serverConfig := serverTLS(t, f)
	for _, tc := range []tlsCase{
		{"valid", func(*installer.Profile) {}, `{"ready":true,"version":"unset"}`, 200, true},
		{"wrong name", func(p *installer.Profile) { p.ServerURL = "https://other.oberth.ci" }, `{"ready":true,"version":"unset"}`, 200, false},
		{"wrong pin", func(p *installer.Profile) { p.CAPin = "other" }, `{"ready":true,"version":"unset"}`, 200, false},
		{"not ready", func(*installer.Profile) {}, `{"ready":false,"version":"unset"}`, 200, false},
		{"starting returns 503", func(*installer.Profile) {}, `{"ready":false,"version":"unset"}`, 503, false},
		{"redirect", func(*installer.Profile) {}, `{"ready":true,"version":"unset"}`, 302, false},
		{"wrong version", func(*installer.Profile) {}, `{"ready":true,"version":"other"}`, 200, false},
		{"body limit", func(*installer.Profile) {}, sizedStatus(1 << 20), 200, true},
		{"body over limit", func(*installer.Profile) {}, sizedStatus(1<<20 + 1), 200, false},
		{"invalid body", func(*installer.Profile) {}, `invalid`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifyTLSCase(t, serverConfig, r.Profile, tc)
		})
	}
}

func serverTLS(t *testing.T, f fixture) *tls.Config {
	t.Helper()
	secret, err := f.client.CoreV1().Secrets("oberth").Get(context.Background(), "oberth-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	caBlock, _ := pem.Decode(secret.Data["tls.crt"])
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(secret.Data["tls.key"])
	caKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := f.engine.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(123), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), DNSNames: []string{"server.oberth.ci"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	serverConfig := &tls.Config{MinVersion: tls.VersionTLS12, Time: f.engine.Now, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	return serverConfig
}

type tlsCase struct {
	name   string
	change func(*installer.Profile)
	body   string
	status int
	ok     bool
}

func verifyTLSCase(t *testing.T, serverConfig *tls.Config, profile installer.Profile, tc tlsCase) {
	t.Helper()
	p := profile
	tc.change(&p)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var workers sync.WaitGroup
	var dials atomic.Int32
	verifier := installer.EndpointVerifier{Now: serverConfig.Time, DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
		client, remote := net.Pipe()
		dials.Add(1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			stop := context.AfterFunc(ctx, func() { _ = remote.Close() })
			defer stop()
			serveTLSCase(ctx, remote, serverConfig, tc)
		}()
		return client, nil
	}}
	err := verifier.Verify(ctx, p)
	workers.Wait()
	if (err == nil) != tc.ok {
		t.Fatal(tc.name, err)
	}
	if tc.name == "wrong version" {
		assertVersionMismatch(t, err, p.ServerVersion)
	}
	if tc.name == "starting returns 503" {
		const want = "The server is not ready yet.\n" +
			"Wait for server recovery to finish; installation retries within its deadline."
		if err == nil || err.Error() != want {
			t.Fatal("startup response lost its retry guidance", err)
		}
	}
	if tc.name == "redirect" && (dials.Load() != 1 || err == nil || !strings.Contains(err.Error(), "HTTP 302")) {
		t.Fatal("redirect was followed instead of refused as HTTP 302", dials.Load(), err)
	}
}

func assertVersionMismatch(t *testing.T, err error, expected string) {
	t.Helper()
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "ready-version" || !strings.Contains(refusal.Detail, "Server version: other") || !strings.Contains(refusal.Detail, "Reviewed release: "+expected) {
		t.Fatal("ready version mismatch lost its classification or versions", err)
	}
}

func serveTLSCase(ctx context.Context, remote net.Conn, config *tls.Config, tc tlsCase) {
	pipe := bufferTLS(remote)
	defer func() { _ = pipe.Close() }()
	server := tls.Server(pipe, config)
	if err := server.HandshakeContext(ctx); err != nil {
		return
	}
	request, err := http.ReadRequest(bufio.NewReader(server))
	if err != nil {
		return
	}
	_ = request.Body.Close()
	body := tc.body
	if request.URL.Path == "/readyz" {
		body = `{"ready":true}`
	}
	header := ""
	if tc.status == 302 {
		header = "Location: https://redirect.oberth.ci/elsewhere\r\n"
	}
	_, _ = fmt.Fprintf(server, "HTTP/1.1 %d Status\r\n%sContent-Length: %d\r\nConnection: close\r\n\r\n%s", tc.status, header, len(body), body)
	pipe.finish()
}

func sizedStatus(n int) string {
	body := `{"ready":true,"version":"unset"}`
	return body + strings.Repeat(" ", n-len(body))
}

func TestServedLeafMustChainToPinnedCA(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := apply(t, f, prepare(t, f))
	r.Profile.ServerURL = "https://server.oberth.ci"
	config := serverTLS(t, f)
	certificate := config.Certificates[0]
	template, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	signer := certificate.PrivateKey.(ed25519.PrivateKey)
	template.SignatureAlgorithm = x509.PureEd25519
	raw, err := x509.CreateCertificate(rand.Reader, template, template, signer.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	config.Certificates = []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: signer}}
	verifyTLSCase(t, config, r.Profile, tlsCase{name: "untrusted server chain", change: func(*installer.Profile) {}, body: `{"ready":true,"version":"unset"}`, status: 200, ok: false})
}
