package githubfake

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHBootstrapAndRepositoryProbe(t *testing.T) {
	g := gitSetup(t)
	// Host-key scan receives the key before deliberately failing authentication.
	sawKey := false
	client, err := ssh.Dial("tcp", g.address, &ssh.ClientConfig{User: "git", HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		sawKey = bytes.Equal(key.Marshal(), g.f.signers[0].PublicKey().Marshal())
		return nil
	}, Timeout: time.Second})
	if client != nil {
		client.Close()
	}
	if err == nil || !sawKey {
		t.Fatalf("host-key scan: %v %v", sawKey, err)
	}
	// upstream_bootstrap_test.go:241 authenticates without opening a channel.
	client, err = dial(t, g, g.sign, g.f.signers[0].PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	client, err = dial(t, g, signer(t), g.f.signers[0].PublicKey())
	if client != nil {
		client.Close()
	}
	if err == nil {
		t.Fatal("unregistered key accepted")
	}
	client, err = dial(t, g, g.sign, signer(t).PublicKey())
	if client != nil {
		client.Close()
	}
	if err == nil {
		t.Fatal("wrong host pin accepted")
	}
}
func TestSSHRepositoryProbe(t *testing.T) {
	g := gitSetup(t)
	client, err := dial(t, g, g.sign, g.f.signers[0].PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	in, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start("git-upload-pack '/org/oberth.git'"); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(out, header); err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.ParseUint(string(header), 16, 16); err != nil {
		t.Fatal(err)
	}
	_, _ = in.Write([]byte("0000"))
	in.Close()
	// Empty repository advertisements are valid too (upstream_repo_probe.go:111).
	if state(t, g.f).UnexpectedCount != 0 {
		t.Fatal(state(t, g.f))
	}
}

// The refusal needles are ref-old/internal/pushretry/retry.go:48 and :63;
// the archived needle is internal/gitcache/branch_deletion.go:439.
func TestSSHInjectedErrors(t *testing.T) {
	g := gitSetup(t)
	for _, tc := range []struct{ kind, want string }{{"not_found", "repository not found"}, {"forbidden", "permission denied"}, {"server_error", "internal server error"}, {"archived", "repository was archived so it is read-only"}, {"protected", "protected branch"}, {"drop", ""}} {
		t.Run(tc.kind, func(t *testing.T) {
			if err := g.f.Inject(Fault{Method: "git-receive-pack", Path: "org/oberth", Kind: tc.kind, Remaining: 1}); err != nil {
				t.Fatal(err)
			}
			out, err := g.command(g.work, "push", "upstream", "HEAD:refs/heads/fault")
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("fault missing: %v %s", err, out)
			}
			g.git(t, g.work, "push", "upstream", "HEAD:refs/heads/fault")
		})
	}
	if err := g.f.Inject(Fault{Method: "auth", Path: "*", Kind: "forbidden", Remaining: 1}); err != nil {
		t.Fatal(err)
	}
	client, err := dial(t, g, g.sign, g.f.signers[0].PublicKey())
	if client != nil {
		client.Close()
	}
	if err == nil {
		t.Fatal("injected auth refusal succeeded")
	}
	client, err = dial(t, g, g.sign, g.f.signers[0].PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if state(t, g.f).UnexpectedCount != 0 {
		t.Fatal(state(t, g.f))
	}
}
func TestSSHSlowFaultWaitsForRelease(t *testing.T) {
	gate := newDelayGate()
	defer gate.open()
	g := gitSetupWithConfig(t, Config{Delay: gate.wait})
	if err := g.f.Inject(Fault{Method: "git-upload-pack", Path: "org/oberth", Kind: "slow", Remaining: 1, DelayMS: 100}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := g.command(g.work, "ls-remote", "upstream"); done <- err }()
	gate.await(t, 100*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Git completed before delay release: %v", err)
	default:
	}
	gate.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if state(t, g.f).UnexpectedCount != 0 {
		t.Fatal("expected slow request counted as unexpected")
	}
}
func TestSSHDeployKeyIsolationAndUnknown(t *testing.T) {
	g := gitSetup(t)
	seed(t, g.f, Seed{Repositories: []RepositorySeed{{Name: "org/other"}}})
	server := httptest.NewTLSServer(g.f)
	defer server.Close()
	readOnly := signer(t)
	status, _, _ := request(t, server.Client(), testRequest{"POST", server.URL + "/api/v3/repos/org/oberth/keys", map[string]any{"title": "Oberth", "key": string(ssh.MarshalAuthorizedKey(readOnly.PublicKey())), "read_only": true}})
	if status != 201 {
		t.Fatal(status)
	}
	for _, tc := range []struct{ command, want string }{{"git-upload-pack '/org/other.git'", "repository not found"}, {"git-receive-pack '/org/oberth.git'", "permission denied"}, {"git-upload-pack '/org/missing.git'", "repository not found"}} {
		c, err := dial(t, g, readOnly, g.f.signers[0].PublicKey())
		if err != nil {
			t.Fatal(err)
		}
		session, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		out, err := session.CombinedOutput(tc.command)
		session.Close()
		c.Close()
		if err == nil || !bytes.Contains(out, []byte(tc.want)) {
			t.Fatalf("isolation: %v %s", err, out)
		}
	}
}
func TestSSHHomeRelativePathAndUnknownCommand(t *testing.T) {
	g := gitSetup(t)
	c, err := dial(t, g, g.sign, g.f.signers[0].PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	input, _ := session.StdinPipe()
	out, _ := session.StdoutPipe()
	if err := session.Start("git-upload-pack '~/org/oberth.git'"); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(out, header); err != nil {
		t.Fatal(err)
	}
	_, _ = input.Write([]byte("0000"))
	input.Close()
	session.Close()
	session, err = c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.Run("arbitrary command") == nil {
		t.Fatal("unknown command accepted")
	}
	if state(t, g.f).UnexpectedCount != 1 {
		t.Fatal(state(t, g.f))
	}
}

func TestSSHShutdownCancelsActiveServices(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(strconv.FormatBool(slow), func(t *testing.T) { assertSSHShutdown(t, slow) })
	}
}
func assertSSHShutdown(t *testing.T, slow bool) {
	t.Helper()
	gate := newDelayGate()
	defer gate.open()
	f := configuredFake(t, Config{Delay: gate.wait})
	key := signer(t)
	seed(t, f, Seed{Repositories: []RepositorySeed{{Name: "org/oberth"}}, AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(key.PublicKey()))}})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.ServeSSH(ctx, listener) }()
	g := &gitFixture{f: f, address: listener.Addr().String()}
	client, err := dial(t, g, key, f.signers[0].PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if slow {
		if err := f.Inject(Fault{Method: "git-receive-pack", Path: "org/oberth", Kind: "slow", Remaining: 1, DelayMS: 60000}); err != nil {
			t.Fatal(err)
		}
	}
	session := receiveSession(t, client, slow)
	defer session.Close()
	if slow {
		gate.await(t, time.Minute)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown left an active service")
	}
}
func receiveSession(t *testing.T, client *ssh.Client, slow bool) *ssh.Session {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start("git-receive-pack '/org/oberth.git'"); err != nil {
		t.Fatal(err)
	}
	if !slow {
		header := make([]byte, 4)
		if _, err := io.ReadFull(stdout, header); err != nil {
			t.Fatal(err)
		}
	}
	return session
}

type handshakeConn struct {
	net.Conn
	deadline time.Time
}

func (c *handshakeConn) SetDeadline(deadline time.Time) error {
	c.deadline = deadline
	return c.Conn.SetDeadline(deadline)
}
func TestSSHHandshakeUsesSuppliedClock(t *testing.T) {
	f := configuredFake(t, Config{Now: fixtureTime})
	server, client := net.Pipe()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	conn := &handshakeConn{Conn: server}
	f.serveConnection(context.Background(), conn)
	if !conn.deadline.Equal(fixtureTime().Add(10 * time.Second)) {
		t.Fatal("handshake deadline did not use supplied clock")
	}
}
