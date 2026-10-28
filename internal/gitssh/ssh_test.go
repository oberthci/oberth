package gitssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestClosedArguments(t *testing.T) {
	for _, args := range [][]string{
		nil, {"-G", "host"}, {"-F", "file", "host", "command"}, {"-o", "ProxyCommand=evil", "host", "command"},
		{"-p", "0", "host", "command"}, {"-p", "65536", "host", "command"}, {"-p", "-1", "host", "command"},
		{"-host", "command"}, {"git@@host", "command"}, {"a b", "command"}, {"host", "bad\ncommand"}, {"host", "command", "extra"},
	} {
		if _, err := parse(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	for _, args := range [][]string{{"git@host", "git-upload-pack '/repo'"}, {"-p", "2222", "git@[::1]", "git-receive-pack '/repo'"}} {
		if _, err := parse(args); err != nil {
			t.Errorf("refused %q: %v", args, err)
		}
	}
}

func testKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}

func configFile(t *testing.T, address string, hostKey ssh.PublicKey) string {
	t.Helper()
	root := t.TempDir()
	key, hosts, config := filepath.Join(root, "identity"), filepath.Join(root, "known_hosts"), filepath.Join(root, "config")
	_, private := testKey(t)
	if err := os.WriteFile(key, private, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hosts, []byte(knownhosts.Line([]string{address}, hostKey)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("Host *\n Port %s\n IdentityFile %q\n IdentitiesOnly yes\n BatchMode yes\n StrictHostKeyChecking yes\n GlobalKnownHostsFile /dev/null\n UserKnownHostsFile %q\n", port, key, hosts)
	if err := os.WriteFile(config, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestUnsupportedConfigRefusesBeforeOpeningIncludes(t *testing.T) {
	signer, _ := testKey(t)
	path := configFile(t, "127.0.0.1:2222", signer.PublicKey())
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"Include /private/never-read", "Include=/private/never-read", "Match exec evil", "ProxyCommand evil", "StrictHostKeyChecking no"} {
		if err := os.WriteFile(path, append([]byte(line+"\n"), original...), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := configured(path, endpoint{host: "127.0.0.1"}); err == nil {
			t.Errorf("accepted %s", line)
		}
	}
}

func TestPinnedTransportStreamsAndPreservesExitStatus(t *testing.T) {
	signer, _ := testKey(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := configFile(t, listener.Addr().String(), signer.PublicKey())
	completed := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			completed <- err
			return
		}
		defer raw.Close()
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
		cfg.AddHostKey(signer)
		connection, channels, requests, err := ssh.NewServerConn(raw, cfg)
		if err != nil {
			completed <- err
			return
		}
		defer connection.Close()
		go ssh.DiscardRequests(requests)
		incoming := <-channels
		channel, requests, err := incoming.Accept()
		if err != nil {
			completed <- err
			return
		}
		defer channel.Close()
		request := <-requests
		var command struct{ Command string }
		if request.Type != "exec" || ssh.Unmarshal(request.Payload, &command) != nil || command.Command != "git-upload-pack '/fixture'" {
			completed <- fmt.Errorf("wrong command")
			return
		}
		_ = request.Reply(true, nil)
		body, err := io.ReadAll(channel)
		if err != nil {
			completed <- err
			return
		}
		_, _ = channel.Write(append([]byte("reply:"), body...))
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{23}))
		completed <- nil
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var out, diagnostics bytes.Buffer
	err = Run(ctx, []string{"git@127.0.0.1", "git-upload-pack '/fixture'"}, config, io.NopCloser(strings.NewReader("request")), &out, &diagnostics)
	if ExitCode(err) != 23 || out.String() != "reply:request" {
		t.Fatalf("exit=%d output=%q: %v", ExitCode(err), out.String(), err)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}

func TestHostKeyMismatchRefusesBeforeAuthentication(t *testing.T) {
	offered, _ := testKey(t)
	pinned, _ := testKey(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := configFile(t, listener.Addr().String(), pinned.PublicKey())
	var authentications atomic.Int32
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			authentications.Add(1)
			return nil, nil
		}}
		cfg.AddHostKey(offered)
		_, _, _, _ = ssh.NewServerConn(raw, cfg)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = Run(ctx, []string{"git@127.0.0.1", "git-upload-pack '/fixture'"}, config, io.NopCloser(strings.NewReader("")), io.Discard, io.Discard)
	if err == nil || ExitCode(err) != 255 {
		t.Fatalf("mismatch accepted: %v", err)
	}
	<-completed
	if authentications.Load() != 0 {
		t.Fatal("identity offered before host-key verification")
	}
}

func TestCancelClosesBlockedHandshake(t *testing.T) {
	signer, _ := testKey(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := configFile(t, listener.Addr().String(), signer.PublicKey())
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"git@127.0.0.1", "git-upload-pack '/fixture'"}, config, io.NopCloser(strings.NewReader("")), io.Discard, io.Discard)
	}()
	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("connection not started")
	}
	defer peer.Close()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cause lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled transport hung")
	}
}
