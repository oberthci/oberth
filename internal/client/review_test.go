package client

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestRequestFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		reason string
	}{
		{"dial", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "Cannot reach"},
		{"authority", x509.UnknownAuthorityError{}, "trusted authority"},
		{"expired", x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired}, "expired"},
		{"hostname", x509.HostnameError{Certificate: &x509.Certificate{}, Host: "oberth.local"}, "another name"},
		{"redirect", failure("redirect", nil), "redirect refused"},
		{"cancelled", context.Canceled, "interrupted"},
		{"timeout", context.DeadlineExceeded, "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, tc.cause }), nil)
			_, _, err := c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`))
			var p *publicerr.Error
			if !errors.As(err, &p) || !strings.Contains(p.Reason(), tc.reason) || calls != 1 {
				t.Fatalf("wrong diagnostic: %v (%d calls)", err, calls)
			}
		})
	}
}

func TestLostMutationReplyNamesRead(t *testing.T) {
	for _, name := range []string{"repo_archive", "uplink_add", "run_cancel"} {
		c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) { return nil, io.EOF }), nil)
		_, _, err := c.Call(t.Context(), definition(t, name), json.RawMessage(`{}`))
		var p *publicerr.Error
		if !errors.As(err, &p) || !strings.Contains(p.Remedy(), "oberth "+strings.ReplaceAll(definition(t, name).Check, "_", " ")) || strings.Contains(p.Remedy(), "retry") {
			t.Fatalf("unsafe remedy: %v", err)
		}
	}
}

func TestCancelledWaitCannotSendAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("sent after cancellation")
		}
		return nil, io.EOF
	}), func(context.Context, time.Duration) error { cancel(); return nil })
	_, _, err := c.Call(ctx, definition(t, "repo_list"), json.RawMessage(`{}`))
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("sent after cancellation: %d %v", calls, err)
	}
}

func TestOnlyRestartingRPCIsRetried(t *testing.T) {
	for _, code := range []int{-32001, -32602, -32603} {
		calls := 0
		c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			if calls > 1 {
				return nil, errors.New("stop")
			}
			data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": code, "data": map[string]string{"reason": "Refused.", "remedy": "Read state."}}})
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
		}), func(context.Context, time.Duration) error { return nil })
		_, _, _ = c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`))
		want := 1
		if code == -32001 {
			want = 2
		}
		if calls != want {
			t.Fatalf("code %d: calls %d", code, calls)
		}
	}
}

func TestReplyMediaAndMalformedReasons(t *testing.T) {
	for _, tc := range []struct{ media, body string }{{"text/html", `{}`}, {"text/plain", `{}`}, {"application/json", `not json`}} {
		resp := &http.Response{Header: http.Header{"Content-Type": []string{tc.media}}, Body: io.NopCloser(strings.NewReader(tc.body))}
		if _, err := apicontract.ReadReply(resp, 100); err == nil {
			t.Fatal("invalid media or JSON accepted")
		}
	}
	d := definition(t, "repo_list")
	if err := replyFailure(d, apicontract.ErrReply); strings.Contains(err.Error(), "smaller") {
		t.Fatal("malformed reply blamed on size")
	}
}

func TestLocalFileKindsLinksAndOwnership(t *testing.T) {
	path := privateFile(t, "credential", []byte("value"))
	dir := filepath.Dir(path)
	link := filepath.Join(dir, "relative")
	if err := os.Symlink("credential", link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, fifo, dir} {
		if _, err := ReadPrivate(path); err == nil {
			t.Fatal("nonregular path accepted")
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if owned(foreignInfo{info}, 077) {
		t.Fatal("foreign owner accepted")
	}
}

type foreignInfo struct{ os.FileInfo }

func (f foreignInfo) Sys() any { return &syscall.Stat_t{Uid: uint32(os.Getuid()) + 1} }

func TestPrivateWriteFailureCleansTemporary(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err = replacePrivate(root, "missing/target", []byte("value")); err == nil {
		t.Fatal("rename unexpectedly succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("temporary file remains")
	}
}

func TestPublicFilesPrivateTokensAndMissingPaths(t *testing.T) {
	path := privateFile(t, "file", []byte("public"))
	for _, mode := range []os.FileMode{0644, 0444} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := readFile(path, false); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPrivate(path); err == nil {
			t.Fatal("public token accepted")
		}
	}
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(path, false); err == nil {
		t.Fatal("writable public file accepted")
	}
	missing := filepath.Join(filepath.Dir(path), "missing")
	if _, err := ReadPrivate(missing); err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing diagnostic: %v", err)
	}
}

func TestProfilePinRequiredAndExplicitCredentialsSkipProfile(t *testing.T) {
	base, dir := profileFixture(t)
	if err := os.Remove(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	o, err := Resolve(Options{}, base, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = New(Config{Options: o}); err == nil || !strings.Contains(err.Error(), "ca.crt") {
		t.Fatal("missing profile pin ignored")
	}
	explicit := Options{URL: "https://oberth.local", CAFile: "ca", TokenFile: "token"}
	if err = os.Chmod(base, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err = Resolve(explicit, base, func(string) string { return "" }); err != nil {
		t.Fatal("explicit credentials read profile")
	}
}

func TestEmptyProfileURLAndMissingSelection(t *testing.T) {
	base, dir := profileFixture(t)
	if err := WritePrivate(filepath.Join(dir, "installation.json"), []byte(`{"server_url":""}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProfile(base, ""); err == nil {
		t.Fatal("empty profile URL accepted")
	}
	if _, _, err := loadProfile(base, "missing"); err == nil || !strings.Contains(err.Error(), filepath.Join(base, "missing")) {
		t.Fatal("missing profile not named")
	}
}

func TestClientCertificateNeedsKeyAndTokenReadFailureStopsRequest(t *testing.T) {
	if _, err := transport(Options{ClientCert: "cert"}); err == nil {
		t.Fatal("certificate without key accepted")
	}
	calls := 0
	c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected") }), nil)
	if err := os.Remove(c.tokenFile); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`))
	if err == nil || calls != 0 || !strings.Contains(err.Error(), c.tokenFile) {
		t.Fatalf("token read ignored: %v", err)
	}
}

func TestPinnedPoolExcludesSystemRoots(t *testing.T) {
	cert, _ := certificate(t, 99)
	caPath := privateFile(t, "ca.crt", cert)
	if err := os.Chmod(caPath, 0644); err != nil {
		t.Fatal(err)
	}
	rt, err := transport(Options{CAFile: caPath})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.CloseIdleConnections()
	roots := rt.TLSClientConfig.RootCAs
	expected := x509.NewCertPool()
	if !expected.AppendCertsFromPEM(cert) {
		t.Fatal("invalid test CA")
	}
	if !roots.Equal(expected) {
		t.Fatal("system roots added to pinned pool")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted TLS reached handler") }))
	defer server.Close()
	h := http.Client{Transport: rt, Timeout: time.Second}
	resp, err := h.Get(server.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("unrelated authority accepted: %v", err)
	}
}

func TestLocalFallbackOnlyForLocalDNS(t *testing.T) {
	for _, tc := range []struct {
		address string
		cause   error
		want    bool
	}{
		{"oberth.local:443", &net.DNSError{Err: "missing"}, true},
		{"oberth.ci:443", &net.DNSError{Err: "missing"}, false},
		{"oberth.local:443", syscall.ECONNREFUSED, false},
	} {
		called := false
		dial := func(context.Context, string, string) (net.Conn, error) { return nil, tc.cause }
		fallback := func(context.Context, string, string) (net.Conn, error) {
			called = true
			return nil, errors.New("lookup failed")
		}
		_, _ = dialWithFallback(t.Context(), "tcp", tc.address, dialFunctions{dial, fallback})
		if called != tc.want {
			t.Fatal("wrong fallback host or error class")
		}
	}
}

func TestPrivateModesAreReadOnlyOrReadWrite(t *testing.T) {
	path := privateFile(t, "private", []byte("value"))
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(path); err != nil {
		t.Fatal("private read-only file refused")
	}
	for _, mode := range []os.FileMode{0700, 0200, 0601} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPrivate(path); err == nil {
			t.Fatalf("accepted private mode %o", mode)
		}
	}
}

func TestReplyUsesToolOutputBudget(t *testing.T) {
	c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`"` + strings.Repeat("x", apicontract.RPCReplyBytes(1)) + `"`))}, nil
	}), nil)
	d := definition(t, "repo_list")
	d.OutputBytes = 1
	_, _, err := c.Call(t.Context(), d, json.RawMessage(`{}`))
	var p *publicerr.Error
	if !errors.As(err, &p) || !strings.Contains(p.Reason(), "limit") {
		t.Fatal("tool output budget ignored")
	}
}

func TestPublicProfileDirectoriesAreReadable(t *testing.T) {
	base, dir := profileFixture(t)
	for _, path := range []string{base, dir} {
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "installation.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Options{}, base, func(string) string { return "" }); err != nil {
		t.Fatal("public profile refused")
	}
}

func TestLostReadRetriesResetAndRestartDial(t *testing.T) {
	for _, cause := range []error{io.EOF, &net.OpError{Op: "read", Err: syscall.ECONNRESET}} {
		calls, waits := 0, 0
		c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			switch calls {
			case 1:
				return nil, cause
			case 2:
				return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
			default:
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{}}}`))}, nil
			}
		}), func(context.Context, time.Duration) error { waits++; return nil })
		_, refused, err := c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`))
		if err != nil || refused || calls != 3 || waits != 2 {
			t.Fatalf("lost read did not reconnect: %d %d %v", calls, waits, err)
		}
	}
}
