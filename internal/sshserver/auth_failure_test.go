package sshserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
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
	"github.com/oberthci/oberth/internal/testtimeout"
)

type authTestLogger struct {
	mu    sync.Mutex
	lines string
	calls int
}

func (l *authTestLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines += fmt.Sprintf(format, args...)
	l.calls++
}

func TestLookupFailuresAbortAuthenticationAsServerErrors(t *testing.T) {
	for _, stage := range []int32{1, 2} {
		for _, failure := range []error{context.DeadlineExceeded, errors.New("database unavailable")} {
			t.Run(fmt.Sprintf("stage-%d-%v", stage, failure), func(t *testing.T) {
				logger := &authTestLogger{}
				var calls atomic.Int32
				server, err := New(Config{HostSigner: newSigner(t), Git: &fakeGit{}, OnUpdate: noOpUpdateHandler(), Logger: logger, MaxConnections: 1,
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
				// Wait for the handler's deferred slot release before cancellation,
				// so Serve has finished logging while its context is still live.
				select {
				case server.connections <- struct{}{}:
					release(server.connections)
				case <-time.After(testtimeout.Start(5 * time.Second)):
					t.Fatal("outage handler did not release its connection slot")
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				logger.mu.Lock()
				defer logger.mu.Unlock()
				if logger.calls != 1 || !strings.Contains(logger.lines, failure.Error()) || !strings.Contains(logger.lines, "SSH identity lookup unavailable") {
					t.Fatalf("want one outage log, got %d: %s", logger.calls, logger.lines)
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

func TestDurableKeyAuthDuringWriterHoldAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.sqlite")
	db, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := db.OpenSSHAuthReaders(t.Context()); err != nil {
		t.Fatal(err)
	}
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
	raw, err := sql.Open("sqlite", path+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	tx, err := raw.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Hold a real SQLite writer independently of the audit gate callback,
	// which deliberately performs external I/O without reserving the writer.
	func() {
		defer func() { _ = tx.Rollback() }()
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
	commandCtx, stop := context.WithTimeout(t.Context(), testtimeout.Start(10*time.Second))
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
