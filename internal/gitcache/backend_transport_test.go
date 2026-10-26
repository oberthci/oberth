package gitcache

import (
	"bytes"
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
	"golang.org/x/crypto/ssh/knownhosts"
)

func testSSHSigner(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}

type gitSSHFixture struct {
	url       string
	hostKey   ssh.PublicKey
	authCalls atomic.Int64
}

func startGitSSHFixture(t *testing.T, repo string, identity ssh.PublicKey) *gitSSHFixture {
	t.Helper()
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git required for the upstream fixture")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, _ := testSSHSigner(t)
	fixture := &gitSSHFixture{url: "ssh://git@" + listener.Addr().String() + repo, hostKey: host.PublicKey()}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		fixture.authCalls.Add(1)
		if !bytes.Equal(identity.Marshal(), key.Marshal()) {
			return nil, errors.New("identity refused")
		}
		return nil, nil
	}}
	config.AddHostKey(host)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Go(func() {
				defer func() { _ = conn.Close() }()
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				server, chans, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				go ssh.DiscardRequests(requests)
				for channel := range chans {
					if channel.ChannelType() != "session" {
						_ = channel.Reject(ssh.UnknownChannelType, "session required")
						continue
					}
					stream, requests, err := channel.Accept()
					if err != nil {
						return
					}
					func() {
						defer func() { _ = stream.Close() }()
						for request := range requests {
							if request.Type != "exec" {
								_ = request.Reply(false, nil)
								continue
							}
							var command struct{ Value string }
							if ssh.Unmarshal(request.Payload, &command) != nil {
								_ = request.Reply(false, nil)
								return
							}
							service := ""
							for _, name := range []string{"upload-pack", "receive-pack"} {
								if command.Value == "git-"+name+" '"+repo+"'" {
									service = name
								}
							}
							if service == "" {
								_ = request.Reply(false, nil)
								return
							}
							_ = request.Reply(true, nil)
							process := exec.CommandContext(ctx, gitBinary, service, repo)
							process.Stdin = stream
							process.Stdout = stream
							process.Stderr = stream.Stderr()
							err := process.Run()
							status := uint32(0)
							if err != nil {
								status = 1
							}
							_, _ = stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			})
		}
	})
	t.Cleanup(func() { cancel(); _ = listener.Close(); workers.Wait() })
	return fixture
}

func TestGoGitPinnedSSHTransportWithoutGitInPATH(t *testing.T) {
	repository := newTestRepository(t)
	identity, body := testSSHSigner(t)
	fixture := startGitSSHFixture(t, repository.upstream, identity.PublicKey())
	root := t.TempDir()
	key := filepath.Join(root, "identity")
	hosts := filepath.Join(root, "known_hosts")
	if err := os.WriteFile(key, body, 0o600); err != nil {
		t.Fatal(err)
	}
	address := strings.TrimPrefix(strings.TrimSuffix(fixture.url, repository.upstream), "ssh://git@")
	line := knownhosts.Line([]string{knownhosts.HashHostname(knownhosts.Normalize(address))}, fixture.hostKey) + "\n"
	if err := os.WriteFile(hosts, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, err := New(Config{Root: filepath.Join(root, "cache"), Backend: BackendGoGit, SSH: SSHConfig{PrivateKeyPath: key, KnownHostsPath: hosts}, Upstream: func(string) (string, error) { return fixture.url, nil }, CommandTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	sha, err := cache.RefSHA(t.Context(), "example", "main")
	if err != nil || sha != repository.initialSHA {
		t.Fatalf("SSH fetch: %s %v (%s)", sha, err, ready.Path)
	}
	if fixture.authCalls.Load() == 0 {
		t.Fatal("SSH identity not exercised")
	}
	// A different pin must fail before public-key authentication. No key is
	// learned or persisted from the failed handshake.
	replacement, _ := testSSHSigner(t)
	bad := knownhosts.Line([]string{address}, replacement.PublicKey()) + "\n"
	if err := os.WriteFile(hosts, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := fixture.authCalls.Load()
	_, err = cache.LsRemoteHeads(t.Context(), "example")
	if err == nil || !strings.Contains(err.Error(), "key mismatch") {
		t.Fatalf("mismatched host accepted: %v", err)
	}
	if fixture.authCalls.Load() != calls {
		t.Fatal("identity offered before host-key verification")
	}
	actual, err := os.ReadFile(hosts)
	if err != nil || string(actual) != bad {
		t.Fatal("host mismatch modified known_hosts")
	}
}

func TestGoGitSSHHandshakeHonorsDeadline(t *testing.T) {
	identity, body := testSSHSigner(t)
	_ = identity
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	var workers sync.WaitGroup
	workers.Go(func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(io.Discard, conn)
	})
	root := t.TempDir()
	key := filepath.Join(root, "identity")
	hosts := filepath.Join(root, "known_hosts")
	host, _ := testSSHSigner(t)
	if err := os.WriteFile(key, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hosts, []byte(knownhosts.Line([]string{listener.Addr().String()}, host.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, err := New(Config{Root: filepath.Join(root, "cache"), Backend: BackendGoGit, SSH: SSHConfig{PrivateKeyPath: key, KnownHostsPath: hosts}, Upstream: func(string) (string, error) { return fmt.Sprintf("ssh://git@%s/repo.git", listener.Addr()), nil }, CommandTimeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = cache.LsRemoteHeads(t.Context(), "example")
	if err == nil {
		t.Fatal("stalled handshake succeeded")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("handshake ignored deadline")
	}
	workers.Wait()
}

func TestGoGitSSHFailureRetainsForgeReason(t *testing.T) {
	identity, body := testSSHSigner(t)
	missing := filepath.Join(t.TempDir(), "missing-repository.git")
	fixture := startGitSSHFixture(t, missing, identity.PublicKey())
	root := t.TempDir()
	key := filepath.Join(root, "identity")
	hosts := filepath.Join(root, "known_hosts")
	if err := os.WriteFile(key, body, 0o600); err != nil {
		t.Fatal(err)
	}
	address := strings.TrimPrefix(strings.TrimSuffix(fixture.url, missing), "ssh://git@")
	if err := os.WriteFile(hosts, []byte(knownhosts.Line([]string{address}, fixture.hostKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, err := New(Config{Root: filepath.Join(root, "cache"), Backend: BackendGoGit, SSH: SSHConfig{PrivateKeyPath: key, KnownHostsPath: hosts}, Upstream: func(string) (string, error) { return fixture.url, nil }, CommandTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cache.LsRemoteHeads(t.Context(), "example")
	if err == nil || !strings.Contains(err.Error(), "does not appear to be a git repository") {
		t.Fatalf("lost upstream refusal reason: %v", err)
	}
}
