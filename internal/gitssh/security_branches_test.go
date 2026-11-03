package gitssh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/testtimeout"
)

// A real SSH peer records whether refused inputs ever disclose an identity or
// start a remote command. It accepts one connection and exits with its client.
type securitySSHPeer struct {
	address         string
	authentications atomic.Int32
	commands        atomic.Int32
	started         chan struct{}
}

func newSecuritySSHPeer(t *testing.T, signer ssh.Signer, algorithms []string, hold bool) *securitySSHPeer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if algorithms != nil {
		signer, err = ssh.NewSignerWithAlgorithms(signer.(ssh.AlgorithmSigner), algorithms)
		if err != nil {
			t.Fatal(err)
		}
	}
	peer := &securitySSHPeer{address: listener.Addr().String(), started: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		defer stop()
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			peer.authentications.Add(1)
			return nil, nil
		}}
		cfg.AddHostKey(signer)
		connection, channels, requests, err := ssh.NewServerConn(raw, cfg)
		if err != nil {
			return
		}
		defer connection.Close()
		go ssh.DiscardRequests(requests)
		for incoming := range channels {
			channel, requests, err := incoming.Accept()
			if err != nil {
				return
			}
			for request := range requests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				peer.commands.Add(1)
				_ = request.Reply(true, nil)
				if hold {
					_, _ = io.Copy(io.Discard, channel)
					peer.started <- struct{}{}
					// EOF on stdin is not cancellation of an active Git service.
					_ = connection.Wait()
				} else {
					_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
				}
				_ = channel.Close()
				return
			}
			_ = channel.Close()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(testtimeout.Start(5 * time.Second)):
			t.Error("SSH fixture did not stop")
		}
	})
	return peer
}

func securityRun(ctx context.Context, config string) error {
	return Run(ctx, []string{"git@127.0.0.1", "git-upload-pack '/fixture'"}, config,
		io.NopCloser(strings.NewReader("")), io.Discard, io.Discard)
}

func TestWorldReadableIdentityNeverAuthenticates(t *testing.T) {
	signer, _ := testKey(t)
	peer := newSecuritySSHPeer(t, signer, nil, false)
	config := configFile(t, peer.address, signer.PublicKey())
	if err := os.Chmod(filepath.Join(filepath.Dir(config), "identity"), 0644); err != nil {
		t.Fatal(err)
	}
	err := securityRun(t.Context(), config)
	if err == nil || peer.authentications.Load() != 0 || peer.commands.Load() != 0 {
		t.Fatalf("exposed identity used: err=%v authentications=%d commands=%d", err, peer.authentications.Load(), peer.commands.Load())
	}
}

func TestInvalidProtocolNeverStartsGitService(t *testing.T) {
	signer, _ := testKey(t)
	peer := newSecuritySSHPeer(t, signer, nil, false)
	config := configFile(t, peer.address, signer.PublicKey())
	t.Setenv("GIT_PROTOCOL", "version=2\nINJECTED=value")
	err := securityRun(t.Context(), config)
	if err == nil || peer.commands.Load() != 0 {
		t.Fatalf("invalid protocol started Git: err=%v commands=%d", err, peer.commands.Load())
	}
}

func TestSHA1OnlyHostKeyNeverAuthenticates(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	peer := newSecuritySSHPeer(t, signer, []string{ssh.KeyAlgoRSA}, false)
	config := configFile(t, peer.address, signer.PublicKey())
	err = securityRun(t.Context(), config)
	if err == nil || peer.authentications.Load() != 0 || peer.commands.Load() != 0 {
		t.Fatalf("SHA-1 host key accepted: err=%v authentications=%d commands=%d", err, peer.authentications.Load(), peer.commands.Load())
	}
}

func TestCancellationStopsActiveGitService(t *testing.T) {
	signer, _ := testKey(t)
	peer := newSecuritySSHPeer(t, signer, nil, true)
	config := configFile(t, peer.address, signer.PublicKey())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- securityRun(ctx, config) }()
	select {
	case <-peer.started:
	case <-time.After(testtimeout.Start(5 * time.Second)):
		t.Fatal("Git service never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation cause lost: %v", err)
		}
	case <-time.After(testtimeout.Start(2 * time.Second)):
		t.Fatal("canceled Git service retained its SSH connection")
	}
}
