package gitssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// A connection the pinned server never answered, ended before Git began or
// lost mid-transfer says so and says to try again; a host-key or identity
// refusal keeps its own words.
func TestInterruptedConnectionsSayToTryAgain(t *testing.T) {
	pinned, other, typed := newSigner(t), newSigner(t), rsaSigner(t)
	mismatch := "Verify the server fingerprint and correct the pinned host key."
	for _, tc := range []struct {
		name, command, reason, remedy string
		server                        func(net.Conn)
		revoked                       ssh.PublicKey
	}{
		{"host key of another type", "git-receive-pack 'r.git'", HostKeyMismatchReason, mismatch, serveThenDrop(typed, true), nil},
		{"host key revoked in the pins", "git-receive-pack 'r.git'", HostKeyMismatchReason, mismatch, serveThenDrop(other, true), other.PublicKey()},
		{"server did not answer", "git-receive-pack 'r.git'", "The Git server did not answer.", "Push again in a moment; the server may be restarting.", nil, nil},
		{"handshake ended", "git-upload-pack 'r.git'", "The Git server ended the connection before Git began.", "Fetch again in a moment; if it repeats, ask the administrator to check this identity.", func(c net.Conn) { _ = c.Close() }, nil},
		{"went away mid-transfer", "git-receive-pack 'r.git'", "The Git server went away during the transfer.", "Push again in a moment; the server may be restarting.", serveThenDrop(pinned, true), nil},
		{"host key differs", "git-receive-pack 'r.git'", HostKeyMismatchReason, "Verify the server fingerprint and correct the pinned host key.", serveThenDrop(other, true), nil},
		{"identity refused", "git-receive-pack 'r.git'", "Git SSH identity was refused.", "Use an active identity; ask the administrator to check revocation and access.", serveThenDrop(pinned, false), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := shimConfig(t, pinned.PublicKey(), tc.command)
			if tc.revoked != nil {
				revoke(t, c.KnownHosts, tc.revoked)
			}
			c.Dial = func(context.Context, string) (net.Conn, error) {
				if tc.server == nil {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
				}
				return loopback(t, tc.server)
			}
			err := Run(t.Context(), c, IO{Input: io.NopCloser(strings.NewReader("")), Output: io.Discard, Diagnostics: io.Discard})
			var problem *publicerr.Error
			if !errors.As(err, &problem) || problem.Reason() != tc.reason || problem.Remedy() != tc.remedy {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// loopback hands serve the accepted end of one local TCP connection; SSH needs
// buffered transport, which an in-memory pipe does not give.
func loopback(t *testing.T, serve func(net.Conn)) (net.Conn, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		if c, err := listener.Accept(); err == nil {
			serve(c)
		}
	}()
	return net.Dial("tcp", listener.Addr().String())
}

// revoke adds a @revoked line for key at the shim's endpoint to the pin file.
func revoke(t *testing.T, path string, key ssh.PublicKey) {
	t.Helper()
	pins, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer pins.Close()
	if _, err = pins.WriteString("@revoked " + knownhosts.Line([]string{"[oberth.test]:2222"}, key) + "\n"); err != nil {
		t.Fatal(err)
	}
}

func rsaSigner(t *testing.T) ssh.Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func shimConfig(t *testing.T, host ssh.PublicKey, command string) Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	identity, known := filepath.Join(dir, "identity"), filepath.Join(dir, "known_hosts")
	pins := knownhosts.Line([]string{"[oberth.test]:2222"}, host) + "\n"
	for path, data := range map[string][]byte{identity: pem.EncodeToMemory(block), known: []byte(pins)} {
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Socket deadlines follow wall time; a far-future clock keeps them open.
	now := time.Unix(4102444800, 0)
	return Config{Host: "oberth.test", Port: "2222", User: "git", Command: command, Identity: identity, KnownHosts: known, Scratch: dir, Now: func() time.Time { return now }}
}

// serveThenDrop completes the handshake with key and, when admit is true,
// accepts one session and its exec request, then drops the connection.
func serveThenDrop(key ssh.Signer, admit bool) func(net.Conn) {
	return func(c net.Conn) {
		defer c.Close()
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			if !admit {
				return nil, errors.New("unregistered")
			}
			return nil, nil
		}}
		cfg.AddHostKey(key)
		_, channels, requests, err := ssh.NewServerConn(c, cfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(requests)
		incoming, open := <-channels
		if !open {
			return
		}
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			return
		}
		request := <-channelRequests
		_ = request.Reply(true, nil)
		_ = channel.Close()
	}
}
