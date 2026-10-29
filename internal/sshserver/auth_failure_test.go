package sshserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type authTestLogger struct {
	mu    sync.Mutex
	lines string
}

func (l *authTestLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines += fmt.Sprintf(format, args...)
}

func TestLookupFailuresAbortAuthenticationAsServerErrors(t *testing.T) {
	for _, stage := range []int32{1, 2} {
		for _, failure := range []error{context.DeadlineExceeded, errors.New("database unavailable")} {
			t.Run(fmt.Sprintf("stage-%d-%v", stage, failure), func(t *testing.T) {
				logger := &authTestLogger{}
				var calls atomic.Int32
				server, err := New(Config{HostSigner: newSigner(t), Git: &fakeGit{}, OnUpdate: noOpUpdateHandler(), Logger: logger,
					Resolver: IdentityResolverFunc(func(string) (string, bool, error) {
						if calls.Add(1) == stage {
							return "", false, failure
						}
						return "registered", true, nil
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- server.Serve(ctx, listener) }()
				var banners string
				config := clientConfig(newSigner(t))
				config.BannerCallback = func(message string) error { banners += message; return nil }
				client, err := ssh.Dial("tcp", listener.Addr().String(), config)
				if client != nil {
					_ = client.Close()
					t.Fatal("lookup outage authenticated")
				}
				if err == nil || strings.Contains(err.Error(), "unable to authenticate") {
					t.Fatalf("outage reported as auth denial: %v", err)
				}
				if !strings.Contains(banners, "temporary server error") || !strings.Contains(banners, "retry later") {
					t.Fatalf("missing transient banner: %q", banners)
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				logger.mu.Lock()
				defer logger.mu.Unlock()
				if !strings.Contains(logger.lines, failure.Error()) {
					t.Fatalf("failure not logged: %s", logger.lines)
				}
			})
		}
	}
}

func TestKeyRevokedAfterProbeCannotAuthenticate(t *testing.T) {
	var revoked atomic.Bool
	server, err := New(Config{HostSigner: newSigner(t), Git: &fakeGit{}, OnUpdate: noOpUpdateHandler(),
		Resolver: IdentityResolverFunc(func(string) (string, bool, error) { return "registered", !revoked.Load(), nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	signer := newSigner(t)
	// Signing runs after the server approves the unsigned probe and before
	// the signed request, allowing a deterministic revoke in between.
	config := clientConfig(signer)
	config.Auth = []ssh.AuthMethod{ssh.PublicKeys(&revokeOnSign{Signer: signer, revoke: func() { revoked.Store(true) }})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	client, err := ssh.Dial("tcp", listener.Addr().String(), config)
	if client != nil {
		_ = client.Close()
		t.Fatal("revoked probe result was cached")
	}
	if err == nil || !revoked.Load() || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("revocation result: %v, revoked=%v", err, revoked.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type revokeOnSign struct {
	ssh.Signer
	revoke func()
}

func (s *revokeOnSign) Sign(random io.Reader, data []byte) (*ssh.Signature, error) {
	s.revoke()
	return s.Signer.Sign(random, data)
}

func TestDurableKeyAuthDuringAuditHoldAndRevocation(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "auth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	signer := newSigner(t)
	credential, err := db.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "ssh", Digest: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.UpsertUplink(t.Context(), model.UplinkSpec{Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), Identity: "ssh-test", TokenCredentialID: credential.ID, AuthActor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ActivateTokenCredential(t.Context(), credential.ID); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{HostSigner: newSigner(t), Git: &fakeGit{}, OnUpdate: noOpUpdateHandler(), Resolver: app.SSHIdentities{Uplinks: db, Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	gateDone := make(chan error, 1)
	go func() {
		_, _, err := db.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, func(model.AuditHead, model.AuditAnchor) error { close(entered); <-release; return nil })
		gateDone <- err
	}()
	<-entered
	// A complete SSH handshake, including both durable lookups, must succeed
	// while the real audit gate owns the writer connection.
	func() {
		defer func() {
			close(release)
			if err := <-gateDone; err != nil {
				t.Error(err)
			}
		}()
		_, stop := startServer(t, server, signer)
		stop()
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	// Commit a real revoke between the unsigned probe and signed auth.
	config := clientConfig(signer)
	config.Auth = []ssh.AuthMethod{ssh.PublicKeys(&revokeOnSign{Signer: signer, revoke: func() {
		if err := db.RevokeTokenCredential(t.Context(), credential.ID); err != nil {
			t.Error(err)
		}
	}})}
	client, err := ssh.Dial("tcp", listener.Addr().String(), config)
	if client != nil {
		_ = client.Close()
		t.Error("key revoked between probe and signature authenticated")
	}
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("revoked auth = %v", err)
	}
	// Every subsequent connection must refuse the same key immediately.
	for range 8 {
		client, err := ssh.Dial("tcp", listener.Addr().String(), clientConfig(signer))
		if client != nil {
			_ = client.Close()
			t.Error("committed revocation authenticated")
		}
		if err == nil {
			t.Error("missing revoked-key refusal")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOpenSSHLookupTimeoutShowsServerError(t *testing.T) {
	executable, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client is unavailable; protocol behavior is also covered by the in-Go client tests")
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "test-only")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{HostSigner: newSigner(t), Git: &fakeGit{}, OnUpdate: noOpUpdateHandler(), Resolver: IdentityResolverFunc(func(string) (string, bool, error) { return "", false, context.DeadlineExceeded })})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	commandCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	output, err := exec.CommandContext(commandCtx, executable, "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentitiesOnly=yes", "-i", keyPath, "-p", port, "git@127.0.0.1", "git-upload-pack 'example.git'").CombinedOutput()
	if err == nil {
		t.Error("OpenSSH authenticated during a lookup timeout")
	}
	if !strings.Contains(string(output), "temporary server error") || strings.Contains(string(output), "Permission denied") {
		t.Errorf("OpenSSH did not report a server outage: %s", output)
	}
	t.Logf("OpenSSH stderr: %s", output)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
