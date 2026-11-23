package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"errors"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
)

// Covers: REQ-C6-006
func TestPinnedGitInteroperabilityContract(t *testing.T) {
	dir := t.TempDir()
	client := privateKey(t, dir, "identity")
	host := privateKey(t, dir, "host")
	alternative, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	alternateSigner, err := ssh.NewSignerFromKey(alternative)
	if err != nil {
		t.Fatal(err)
	}
	pins := filepath.Join(dir, "known_hosts")
	if err = os.WriteFile(pins, append([]byte("[git.example]:2222 "), ssh.MarshalAuthorizedKey(host.PublicKey())...), 0600); err != nil {
		t.Fatal(err)
	}
	left, right := sshtest.Pipe()
	defer func() { _ = left.Close(); _ = right.Close() }()
	done := make(chan error, 1)
	go func() { done <- serveGitCommand(right, client.PublicKey(), []ssh.Signer{alternateSigner, host}) }()
	cfg := gitssh.Config{Host: "git.example", Port: "2222", User: "git", Command: "git-upload-pack 'team/widget.git'", Identity: filepath.Join(dir, "identity"), KnownHosts: pins, Scratch: dir, Now: func() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }, Dial: func(context.Context, string) (net.Conn, error) { return left, nil }}
	var output bytes.Buffer
	err = gitssh.Run(t.Context(), cfg, gitssh.IO{Input: io.NopCloser(strings.NewReader("request")), Output: &output, Diagnostics: io.Discard})
	if err != nil {
		serverErr := <-done
		t.Fatalf("client: %v; server: %v", err, serverErr)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if output.String() != "accepted" {
		t.Fatal(output.String())
	}
	if _, err = gitssh.Parse([]string{"-p", "2222", "git@git.example", "git-upload-pack 'team/widget.git'"}); err != nil {
		t.Fatal(err)
	}
	if _, err = gitssh.Parse([]string{"git.example", "unsafe\ncommand"}); err == nil {
		t.Fatal("invalid transport invocation accepted")
	}
}
func privateKey(t *testing.T, dir, name string) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return signer
}
func serveGitCommand(conn net.Conn, identity ssh.PublicKey, hosts []ssh.Signer) error {
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), identity.Marshal()) {
			return nil, errors.New("test SSH request refused")
		}
		return nil, nil
	}}
	for _, host := range hosts {
		config.AddHostKey(host)
	}
	native, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return err
	}
	defer func() { _ = native.Close() }()
	done := make(chan struct{})
	go func() { ssh.DiscardRequests(requests); close(done) }()
	defer func() { _ = native.Close(); <-done }()
	return serveGitSession(channels)
}
func serveGitSession(channels <-chan ssh.NewChannel) error {
	channel, reqs, err := (<-channels).Accept()
	if err != nil {
		return err
	}
	defer func() { _ = channel.Close() }()
	request := <-reqs
	var command struct{ Command string }
	if ssh.Unmarshal(request.Payload, &command) != nil || request.Type != "exec" || command.Command != "git-upload-pack 'team/widget.git'" {
		return errors.New("test SSH request refused")
	}
	if err = request.Reply(true, nil); err != nil {
		return err
	}
	input, err := io.ReadAll(channel)
	if err != nil || string(input) != "request" {
		return errors.New("test SSH transport failed")
	}
	if _, err = io.WriteString(channel, "accepted"); err != nil {
		return err
	}
	_, err = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
	if err != nil {
		return err
	}
	if err = channel.Close(); err != nil {
		return err
	}
	// The request stream closes after the peer's close reply or transport EOF.
	// Keep the buffered transport alive until then.
	ssh.DiscardRequests(reqs)
	return nil
}
