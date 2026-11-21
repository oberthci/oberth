package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/utils/clock"
)

type commandFixture struct {
	addresses
	client *http.Client
	roots  *x509.CertPool
}

func startCommand(t *testing.T, extra ...string) commandFixture {
	t.Helper()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	args := append([]string{"-root", t.TempDir(), "-ca-out", caFile, "-https", "127.0.0.1:0", "-control", "127.0.0.1:0", "-ssh", "127.0.0.1:0"}, extra...)
	ctx, cancel := context.WithCancel(context.Background())
	bound, done := make(chan addresses, 1), make(chan error, 1)
	go func() { done <- run(ctx, args, func(a addresses) { bound <- a }, clock.RealClock{}.Now) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("listeners did not stop")
		}
	})
	var a addresses
	select {
	case a = <-bound:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("listeners did not start")
	}
	body, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(body) {
		t.Fatal("CA output is not a certificate")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, ForceAttemptHTTP2: true}
	t.Cleanup(transport.CloseIdleConnections)
	return commandFixture{a, &http.Client{Transport: transport, Timeout: time.Second}, roots}
}
func TestCommandListenersTrustAndHostKey(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "host-key")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	f := startCommand(t, "-host-key", keyFile)
	req, _ := http.NewRequest("GET", "https://"+f.HTTPS+"/api/v3/", nil)
	req.Header.Set("Authorization", "token fixture")
	res, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || res.ProtoMajor != 1 {
		t.Fatal("public HTTPS listener differs")
	}
	res, err = f.client.Get("http://" + f.Control + "/_control/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var state struct {
		HostKeys []struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"host_keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.HostKeys) != 1 || state.HostKeys[0].Fingerprint != ssh.FingerprintSHA256(pub) {
		t.Fatal("host key file not used")
	}
}
func TestCommandSeparatesPublicAndControl(t *testing.T) {
	f := startCommand(t)
	for _, url := range []string{"https://" + f.HTTPS + "/_control/state", "http://" + f.Control + "/api/v3/"} {
		res, err := f.client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 501 {
			t.Fatal("public/control surfaces mixed")
		}
	}
}
func TestCommandDropClosesHTTPConnection(t *testing.T) {
	f := startCommand(t)
	res, err := f.client.Post("http://"+f.Control+"/_control/faults", "application/json", strings.NewReader(`{"method":"GET","path":"/api/v3/","kind":"drop","remaining":1}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	conn, err := tls.Dial("tcp", f.HTTPS, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: f.roots, ServerName: "localhost", NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.ConnectionState().NegotiatedProtocol == "h2" {
		t.Fatal("HTTP/2 negotiated")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET /api/v3/ HTTP/1.1\r\nHost: localhost\r\nAuthorization: token fixture\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(conn)
	if err != nil || len(body) != 0 {
		t.Fatalf("drop did not close connection without response: %q %v", body, err)
	}
	assertDropState(t, f)
}
func assertDropState(t *testing.T, f commandFixture) {
	t.Helper()
	res, err := f.client.Get("http://" + f.Control + "/_control/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var state struct {
		Faults          []struct{ Attempts, Remaining int }
		UnexpectedCount int `json:"unexpected_count"`
	}
	if err := json.NewDecoder(res.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if len(state.Faults) != 1 || state.Faults[0].Attempts != 1 || state.Faults[0].Remaining != 0 || state.UnexpectedCount != 0 {
		t.Fatal("drop accounting differs")
	}
}
func TestCommandRefusesInvalidConfiguration(t *testing.T) {
	for _, args := range [][]string{nil, {"-root", "relative"}, {"-root", t.TempDir(), "-ca-out", filepath.Join(t.TempDir(), "ca.pem"), "-control", "0.0.0.0:0", "-https", "127.0.0.1:0", "-ssh", "127.0.0.1:0"}, {"-root", t.TempDir(), "-ca-out", filepath.Join(t.TempDir(), "ca.pem"), "-https", "invalid"}, {"-tls-cert", "missing"}, {"unexpected"}} {
		if err := run(context.Background(), args, nil, clock.RealClock{}.Now); err == nil {
			t.Fatal("invalid flags accepted")
		}
	}
}
func TestCommandFileErrorsNamePathAndCause(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, extra := range [][]string{{"-host-key", missing}, {"-ca-out", filepath.Join(missing, "ca.pem")}, {"-ca-cert", missing, "-ca-key", missing}} {
		args := append([]string{"-root", t.TempDir(), "-ca-out", filepath.Join(t.TempDir(), "ca.pem")}, extra...)
		err := run(context.Background(), args, nil, clock.RealClock{}.Now)
		if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), missing) {
			t.Fatalf("file error lacks path or cause: %v", err)
		}
	}
}
func TestCommandHelp(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestCommandHelpProcess$")
	cmd.Env = append(os.Environ(), "GHFAKE_HELP_TEST=1")
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Usage of githubfake:") || !strings.Contains(string(output), "-root") {
		t.Fatalf("help failed: %v %s", err, output)
	}
}
func TestCommandHelpProcess(t *testing.T) {
	if os.Getenv("GHFAKE_HELP_TEST") != "1" {
		return
	}
	if err := run(context.Background(), []string{"-h"}, nil, clock.RealClock{}.Now); err != nil {
		t.Fatal(err)
	}
}
