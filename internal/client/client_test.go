package client

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const secret = "tok-DO-NOT-LEAK-9f3a"

func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"OBERTH_BASE_URL", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND", "OBERTH_CA_CERT"} {
		t.Setenv(name, "")
	}
}

func TestConfiguredReportsWhetherAServerIsSet(t *testing.T) {
	clearEnv(t)
	if FromEnv().Configured() {
		t.Fatal("configured with no base URL")
	}
	t.Setenv("OBERTH_BASE_URL", "https://oberth.example")
	if !FromEnv().Configured() {
		t.Fatal("not configured with a base URL set")
	}
}

func TestTokenComesFromTheEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBERTH_TOKEN", secret)
	got, err := FromEnv().resolveToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("token = %q", got)
	}
}

func TestTokenCommandStdoutIsTrimmed(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBERTH_TOKEN_COMMAND", "printf '"+secret+"\\n\\n'")
	got, err := FromEnv().resolveToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("token = %q, want the trimmed value", got)
	}
}

func TestTokenCommandFailureSurfacesItsOwnStderr(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBERTH_TOKEN_COMMAND", "echo 'vault is locked' >&2; exit 3")
	_, err := FromEnv().resolveToken(t.Context())
	if err == nil {
		t.Fatal("a failing token command succeeded")
	}
	if !strings.Contains(err.Error(), "vault is locked") {
		t.Fatalf("error does not carry the command's own message: %v", err)
	}
}

func TestAMissingTokenNamesTheEnvironmentVariable(t *testing.T) {
	clearEnv(t)
	_, err := FromEnv().resolveToken(t.Context())
	if err == nil {
		t.Fatal("no token was accepted")
	}
	if !strings.Contains(err.Error(), "OBERTH_TOKEN") {
		t.Fatalf("error does not name the variable to set: %v", err)
	}
}

func newClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	clearEnv(t)
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", secret)
	client, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestGetSendsTheBearerTokenAndDecodes(t *testing.T) {
	var seenAuth, seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth, seenPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ID":"run-1","Status":"passed"}`))
	}))
	defer server.Close()

	var run struct{ ID, Status string }
	if err := newClient(t, server).Get(context.Background(), "/api/runs/run-1", nil, &run); err != nil {
		t.Fatal(err)
	}
	if seenAuth != "Bearer "+secret {
		t.Fatalf("Authorization = %q", seenAuth)
	}
	if seenPath != "/api/runs/run-1" {
		t.Fatalf("path = %q", seenPath)
	}
	if run.ID == "" || run.Status == "" {
		t.Fatalf("decoded to zero values, which is what a snake_case struct would do: %+v", run)
	}
}

func TestGetPassesQueryParameters(t *testing.T) {
	var seenQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	var out struct{}
	query := map[string]string{"pattern": "FAIL", "context": "3"}
	if err := newClient(t, server).Get(context.Background(), "/api/runs/x/logs", query, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pattern=FAIL", "context=3"} {
		if !strings.Contains(seenQuery, want) {
			t.Fatalf("query %q missing %q", seenQuery, want)
		}
	}
}

func TestStatusCodesMapToDistinctMessages(t *testing.T) {
	cases := map[int]string{
		http.StatusUnauthorized:        "token",
		http.StatusForbidden:           "not permitted",
		http.StatusNotFound:            "not found",
		http.StatusInternalServerError: "server",
	}
	seen := map[string]bool{}
	for code, fragment := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"upstream said no"}`))
		}))
		var out struct{}
		err := newClient(t, server).Get(context.Background(), "/api/runs", nil, &out)
		server.Close()
		if err == nil {
			t.Fatalf("%d returned no error", code)
		}
		if !strings.Contains(strings.ToLower(err.Error()), fragment) {
			t.Fatalf("%d gave %q, want something mentioning %q", code, err, fragment)
		}
		if seen[err.Error()] {
			t.Fatalf("%d produced a message already used by another status: %q", code, err)
		}
		seen[err.Error()] = true
	}
}

func TestANonJSONBodyDoesNotPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>gateway timeout</html>"))
	}))
	defer server.Close()

	var out struct{}
	err := newClient(t, server).Get(context.Background(), "/api/runs", nil, &out)
	if err == nil {
		t.Fatal("an HTML error body was accepted")
	}
}

func TestNoFailurePathEverEchoesTheToken(t *testing.T) {
	bodies := []struct {
		code int
		body string
	}{
		{http.StatusUnauthorized, `{"error":"invalid bearer token"}`},
		{http.StatusNotFound, `{"error":"no such run"}`},
		{http.StatusInternalServerError, `{"error":"boom"}`},
		{http.StatusBadGateway, "<html>nope</html>"},
		{http.StatusOK, "not json at all"},
	}
	for _, tc := range bodies {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.code)
			_, _ = w.Write([]byte(tc.body))
		}))
		var out struct{}
		err := newClient(t, server).Get(context.Background(), "/api/runs", nil, &out)
		server.Close()
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("status %d leaked the token: %v", tc.code, err)
		}
	}

	clearEnv(t)
	t.Setenv("OBERTH_BASE_URL", "https://127.0.0.1:1")
	t.Setenv("OBERTH_TOKEN", secret)
	client, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	if err := client.Get(context.Background(), "/api/runs", nil, &out); err != nil {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("a transport error leaked the token: %v", err)
		}
	}
}

func TestAnUntrustedCertificateIsRefusedUntilItsAnchorIsSupplied(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ID":"run-1"}`))
	}))
	defer server.Close()

	clearEnv(t)
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", secret)
	client, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ ID string }
	err = client.Get(context.Background(), "/api/runs/run-1", nil, &out)
	if err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
	if !strings.Contains(err.Error(), "OBERTH_CA_CERT") {
		t.Fatalf("error does not name the variable that would fix it: %v", err)
	}

	anchor := filepath.Join(t.TempDir(), "ca.pem")
	certificate := server.Certificate()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	if err := os.WriteFile(anchor, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OBERTH_CA_CERT", anchor)
	trusted, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	if err := trusted.Get(context.Background(), "/api/runs/run-1", nil, &out); err != nil {
		t.Fatalf("the supplied anchor did not make the certificate trusted: %v", err)
	}
	if out.ID != "run-1" {
		t.Fatalf("decoded %+v", out)
	}
}

func TestTheClientNeverDisablesCertificateVerification(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		body, readErr := os.ReadFile(entry.Name())
		if readErr != nil {
			t.Fatal(readErr)
		}
		checked++
		if strings.Contains(string(body), "InsecureSkipVerify") {
			t.Fatalf("%s mentions InsecureSkipVerify; a CI client that can be told to trust anything will be",
				entry.Name())
		}
	}
	if checked == 0 {
		t.Fatal("no source inspected; the guard would pass vacuously")
	}
}

func TestNewRefusesAMalformedBaseURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBERTH_TOKEN", secret)
	for _, base := range []string{"", "not a url", "ftp://oberth.example", "://broken"} {
		t.Setenv("OBERTH_BASE_URL", base)
		if _, err := New(t.Context(), FromEnv()); err == nil {
			t.Fatalf("base URL %q was accepted", base)
		}
	}
}

func TestAHostnameMismatchNamesTheAddressesTheCertificateCovers(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	anchor := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(anchor, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	clearEnv(t)
	address := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	t.Setenv("OBERTH_BASE_URL", address)
	t.Setenv("OBERTH_TOKEN", secret)
	t.Setenv("OBERTH_CA_CERT", anchor)
	api, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	err = api.Get(context.Background(), "/api/runs", nil, &out)
	if err == nil {
		t.Skip("this certificate covers localhost, so there is no mismatch to observe")
	}
	if strings.Contains(err.Error(), "OBERTH_CA_CERT") {
		t.Fatalf("a hostname mismatch was reported as an untrusted authority, "+
			"which sends the user to fix the wrong thing: %v", err)
	}
	if !strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("error does not explain the mismatch: %v", err)
	}
}

// --- #235: reject non-loopback http:// ---

func TestPlainHTTPIsRejectedForNonLoopbackHosts(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBERTH_TOKEN", secret)
	for _, base := range []string{
		"http://oberth.example",
		"http://10.0.0.1:8443",
		"http://192.168.1.1",
	} {
		t.Setenv("OBERTH_BASE_URL", base)
		_, err := New(t.Context(), FromEnv())
		if err == nil {
			t.Fatalf("http:// to %q was accepted; the bearer token would be in cleartext", base)
		}
		if !strings.Contains(err.Error(), "cleartext") {
			t.Fatalf("error does not say why http:// is refused: %v", err)
		}
	}
}

func TestPlainHTTPIsAllowedForLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	clearEnv(t)
	t.Setenv("OBERTH_BASE_URL", server.URL) // http://127.0.0.1:<port>
	t.Setenv("OBERTH_TOKEN", secret)
	if _, err := New(t.Context(), FromEnv()); err != nil {
		t.Fatalf("http://127.0.0.1 should be allowed for port-forward use: %v", err)
	}
}

// --- #237: GetTo streams raw binary, not JSON ---

func TestGetToStreamsRawBinaryContent(t *testing.T) {
	binary := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a} // PNG header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(binary)
	}))
	defer server.Close()

	client := newClient(t, server)
	var got bytes.Buffer
	if err := client.GetTo(context.Background(), "/api/runs/r/artifacts/img.png", nil, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), binary) {
		t.Fatalf("body = %x, want %x", got.Bytes(), binary)
	}
}

func TestGetToDoesNotSetAcceptJSON(t *testing.T) {
	var seenAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte("data"))
	}))
	defer server.Close()

	client := newClient(t, server)
	_ = client.GetTo(context.Background(), "/test", nil, io.Discard)
	if seenAccept == "application/json" {
		t.Fatal("GetTo set Accept: application/json, but it is for binary downloads")
	}
}

func TestGetToIsNotCappedAtTheJSONResponseSize(t *testing.T) {
	// The server's default artifact ceiling is 256 MiB per run while the
	// JSON read cap is 8 MiB. Before the streaming fix the client returned
	// the first 8 MiB of a larger artifact with no error — a silently
	// corrupt download is worse than a loud one.
	const chunkSize = 64 * 1024
	const size = maxResponseSize + chunkSize
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		chunk := bytes.Repeat([]byte{0xA5}, chunkSize)
		for written := 0; written < size; written += chunkSize {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	client := newClient(t, server)
	var got bytes.Buffer
	if err := client.GetTo(context.Background(), "/api/runs/r/artifacts/big.bin", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Len() != size {
		t.Fatalf("downloaded %d bytes of %d; the transfer was truncated", got.Len(), size)
	}
	if got.Bytes()[size-1] != 0xA5 {
		t.Fatalf("final byte = %02x, want a5", got.Bytes()[size-1])
	}
}

// --- #236: OBERTH_CA_CERT is a pin, TLS floor is 1.3, no silent fallback ---

// mintUnrelatedCA writes a freshly generated self-signed CA certificate to a
// temporary file and returns its path. Every httptest TLS server in a process
// serves the same embedded certificate, so a genuinely unrelated authority
// has to be minted, not borrowed from a second server.
func mintUnrelatedCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "unrelated-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestACACertPinReplacesTheSystemPool(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	anchor := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(anchor, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// White box (the append/replace discriminator): the trust pool is
	// exactly the pinned PEM. CertPool.Equal distinguishes a fresh pool
	// from a system pool with the PEM appended even on a machine whose
	// system store happens to be empty.
	roundTripper, err := newTransport(anchor)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := roundTripper.(*http.Transport)
	if !ok {
		t.Fatalf("newTransport returned %T", roundTripper)
	}
	want := x509.NewCertPool()
	if !want.AppendCertsFromPEM(pemBytes) {
		t.Fatal("the test anchor did not parse")
	}
	if !transport.TLSClientConfig.RootCAs.Equal(want) {
		t.Fatal("OBERTH_CA_CERT did not replace the trust pool; an explicit pin must trust only the named authority")
	}

	// Behaviorally: the pin that names this server's authority verifies it,
	// and a pin naming an unrelated authority rejects it — nothing else
	// about the connection differs between the two.
	clearEnv(t)
	t.Setenv("OBERTH_TOKEN", secret)
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_CA_CERT", anchor)
	trusted, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	if err := trusted.Get(context.Background(), "/api/runs", nil, &out); err != nil {
		t.Fatalf("the pinned authority was not trusted: %v", err)
	}
	t.Setenv("OBERTH_CA_CERT", mintUnrelatedCA(t))
	stranger, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	if err := stranger.Get(context.Background(), "/api/runs", nil, &out); err == nil {
		t.Fatal("a certificate outside the pin was accepted")
	}
}

func TestTLSFloorIsOnePointThreeWithAndWithoutACACert(t *testing.T) {
	bare, err := newTransport("")
	if err != nil {
		t.Fatal(err)
	}
	bareTransport, ok := bare.(*http.Transport)
	if !ok {
		t.Fatalf("newTransport returned %T", bare)
	}
	if bareTransport.TLSClientConfig == nil || bareTransport.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatal("without OBERTH_CA_CERT the TLS floor is not 1.3")
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	server.TLS = &tls.Config{MaxVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	anchor := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(anchor, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	anchored, err := newTransport(anchor)
	if err != nil {
		t.Fatal(err)
	}
	anchoredTransport, ok := anchored.(*http.Transport)
	if !ok {
		t.Fatalf("newTransport returned %T", anchored)
	}
	if anchoredTransport.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatal("with OBERTH_CA_CERT the TLS floor is not 1.3")
	}

	// Behaviorally: a server capped at TLS 1.2 is refused even though its
	// certificate is the pinned anchor, so the only possible failure is the
	// protocol floor.
	clearEnv(t)
	t.Setenv("OBERTH_TOKEN", secret)
	t.Setenv("OBERTH_CA_CERT", anchor)
	t.Setenv("OBERTH_BASE_URL", server.URL)
	api, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	if err := api.Get(context.Background(), "/api/runs", nil, &out); err == nil {
		t.Fatal("a TLS 1.2 handshake was accepted below the 1.3 floor")
	}
}

type opaqueTransport struct{}

func (opaqueTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("opaque")
}

func TestNewErrorsWhenDefaultTransportCannotCarryTLS(t *testing.T) {
	// Deliberately not parallel: it swaps the process-global
	// http.DefaultTransport and restores it before returning.
	saved := http.DefaultTransport
	http.DefaultTransport = opaqueTransport{}
	defer func() { http.DefaultTransport = saved }()

	clearEnv(t)
	t.Setenv("OBERTH_BASE_URL", "https://oberth.example")
	t.Setenv("OBERTH_TOKEN", secret)
	_, err := New(t.Context(), FromEnv())
	if err == nil {
		t.Fatal("a transport that cannot carry TLS configuration was accepted silently")
	}
	if !strings.Contains(err.Error(), "Transport") {
		t.Fatalf("the error does not name the transport problem: %v", err)
	}
}

// --- #242: transport error classification preserves detail ---

func TestTransportErrorPreservesConnectionRefused(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBERTH_BASE_URL", "https://127.0.0.1:1")
	t.Setenv("OBERTH_TOKEN", secret)
	api, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	err = api.Get(context.Background(), "/api/runs", nil, &out)
	if err == nil {
		t.Fatal("a connection to port 1 succeeded")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("transport error lost the connection-refused detail: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("transport error leaked the token: %v", err)
	}
}

func TestClassifyTransportPreservesTimeoutDetail(t *testing.T) {
	got := classifyTransport("/api/runs", context.DeadlineExceeded)
	if !strings.Contains(got.Error(), "timed out") {
		t.Fatalf("timeout detail lost: %v", got)
	}
}

func TestClassifyTransportPreservesDNSDetail(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "missing.invalid", IsNotFound: true}
	inner := &net.OpError{Op: "dial", Net: "tcp", Err: dnsErr}
	got := classifyTransport("/api/runs", inner)
	if !strings.Contains(got.Error(), "resolve") || !strings.Contains(got.Error(), "missing.invalid") {
		t.Fatalf("DNS detail lost: %v", got)
	}
}

func TestARedirectDowngradeToHTTPIsRefusedBeforeSending(t *testing.T) {
	// A plain-HTTP listener records whether any request ever reaches it.
	var cleartextHits int32
	cleartext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&cleartextHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer cleartext.Close()

	// The TLS server downgrade-redirects to the plain-HTTP listener.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cleartext.URL+"/api/runs/run-1", http.StatusFound)
	}))
	defer server.Close()

	anchor := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(anchor, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	clearEnv(t)
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", "bearer-value-must-stay-on-tls")
	t.Setenv("OBERTH_CA_CERT", anchor)
	client, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}

	var out struct{ ID string }
	err = client.Get(context.Background(), "/api/runs/run-1", nil, &out)
	if err == nil {
		t.Fatal("a https->http downgrade redirect was followed")
	}
	if !strings.Contains(err.Error(), "cleartext") {
		t.Fatalf("error does not explain the downgrade refusal: %v", err)
	}
	if got := atomic.LoadInt32(&cleartextHits); got != 0 {
		t.Fatalf("the cleartext listener received %d request(s); the redirect must be refused before sending", got)
	}
}

func TestARedirectWithinHTTPSIsStillFollowed(t *testing.T) {
	// Same-scheme redirects must keep working: the guard closes downgrades,
	// not redirects in general. One TLS server redirects to itself.
	var served int32
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	mux.HandleFunc("/api/runs/run-1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/api/runs/run-1-moved", http.StatusFound)
	})
	mux.HandleFunc("/api/runs/run-1-moved", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&served, 1)
		_, _ = w.Write([]byte(`{"ID":"run-1"}`))
	})

	anchor := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(anchor, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	clearEnv(t)
	t.Setenv("OBERTH_BASE_URL", server.URL)
	t.Setenv("OBERTH_TOKEN", "token-value")
	t.Setenv("OBERTH_CA_CERT", anchor)
	client, err := New(t.Context(), FromEnv())
	if err != nil {
		t.Fatal(err)
	}

	var out struct{ ID string }
	if err := client.Get(context.Background(), "/api/runs/run-1", nil, &out); err != nil {
		t.Fatalf("same-scheme redirect refused: %v", err)
	}
	if out.ID != "run-1" || atomic.LoadInt32(&served) != 1 {
		t.Fatalf("redirect target not served exactly once (ID=%q served=%d)", out.ID, served)
	}
}
