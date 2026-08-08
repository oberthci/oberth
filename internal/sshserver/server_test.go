package sshserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/gitcache"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
)

func TestParseCommandStrictlyAllowsGitServices(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value   string
		service gitcache.Service
		repo    string
	}{
		{"git-upload-pack '/example.git'", gitcache.UploadPack, "example"},
		{"git-receive-pack 'example.git'", gitcache.ReceivePack, "example"},
	}
	for _, test := range tests {
		command, err := ParseCommand(test.value)
		if err != nil {
			t.Fatalf("ParseCommand(%q): %v", test.value, err)
		}
		if command.service != test.service || command.repo != test.repo {
			t.Fatalf("ParseCommand(%q) = %+v", test.value, command)
		}
	}
	for _, value := range []string{
		"git-upload-pack /example.git",
		"git-upload-pack '../../secret'",
		"git-upload-pack 'owner/example.git'",
		"git-upload-pack 'example.git'; id",
		"git-receive-pack 'example.git' extra",
		"sh -c 'git-upload-pack example.git'",
		"git-shell 'example.git'",
		"git-upload-pack 'example.git\nother'",
	} {
		if _, err := ParseCommand(value); err == nil {
			t.Fatalf("ParseCommand(%q) unexpectedly succeeded", value)
		}
	}
}

func TestAuthorizedKeyCanExecuteGitAndPassProtocol(t *testing.T) {
	t.Parallel()
	host := newSigner(t)
	clientSigner := newSigner(t)
	fingerprint := ssh.FingerprintSHA256(clientSigner.PublicKey())
	git := &fakeGit{refs: map[string]string{"refs/heads/main": shaA}}
	server, err := New(Config{
		HostSigner: host,
		Resolver: IdentityResolverFunc(func(got string) (string, bool, error) {
			return "agent@host", got == fingerprint, nil
		}),
		Git:      git,
		OnUpdate: noOpUpdateHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	client, stop := startServer(t, server, clientSigner)
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Setenv("GIT_PROTOCOL", "version=2"); err != nil {
		t.Fatalf("Setenv GIT_PROTOCOL: %v", err)
	}
	if err := session.Run("git-upload-pack '/example.git'"); err != nil {
		t.Fatal(err)
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	if git.service != gitcache.UploadPack || git.protocol != "version=2" || git.repo != "example" {
		t.Fatalf("Git invocation = service %q protocol %q repo %q", git.service, git.protocol, git.repo)
	}
}

func TestUnauthorizedPublicKeyIsRefused(t *testing.T) {
	t.Parallel()
	server, err := New(Config{
		HostSigner: newSigner(t),
		Resolver: IdentityResolverFunc(func(string) (string, bool, error) {
			return "", false, nil
		}),
		Git:      &fakeGit{},
		OnUpdate: noOpUpdateHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(ctx, listener) }()
	_, err = ssh.Dial("tcp", listener.Addr().String(), clientConfig(newSigner(t)))
	if err == nil {
		t.Fatal("unregistered public key unexpectedly authenticated")
	}
	_ = listener.Close()
}

func TestRejectsShellPTYForwardingAndOtherEnvironment(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	server := authorizedServer(t, clientSigner, &fakeGit{})
	client, stop := startServer(t, server, clientSigner)
	defer stop()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err == nil {
		t.Fatal("interactive shell unexpectedly allowed")
	}
	_ = session.Close()

	session, err = client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err == nil {
		t.Fatal("PTY unexpectedly allowed")
	}
	_ = session.Close()

	session, err = client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Setenv("LD_PRELOAD", "/tmp/injected.so"); err == nil {
		t.Fatal("non-protocol environment unexpectedly allowed")
	}
	_ = session.Close()

	connection, err := client.Dial("tcp", "127.0.0.1:1")
	if err == nil {
		_ = connection.Close()
		t.Fatal("TCP forwarding unexpectedly allowed")
	}
}

func TestSuccessfulReceiveCallsUpdateHandlerWithAuthenticatedActor(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	git := &fakeGit{
		refs: map[string]string{"refs/heads/main": shaA},
		afterReceive: map[string]string{
			"refs/heads/main":  shaB,
			"refs/tags/v1.0.0": shaC,
		},
	}
	type observed struct {
		actor  string
		repo   string
		update gitcache.RefUpdate
	}
	var mu sync.Mutex
	var updates []observed
	server, err := New(Config{
		HostSigner: newSigner(t),
		Resolver: IdentityResolverFunc(func(fingerprint string) (string, bool, error) {
			return "worker@tuxbox", fingerprint == ssh.FingerprintSHA256(clientSigner.PublicKey()), nil
		}),
		Git: git,
		OnUpdate: UpdateHandlerFunc(func(_ context.Context, event gitcache.ReceiveEvent) error {
			mu.Lock()
			defer mu.Unlock()
			updates = append(updates, observed{actor: event.Actor, repo: event.Repo, update: event.Update})
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	client, stop := startServer(t, server, clientSigner)
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Run("git-receive-pack '/example.git'"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(updates) != 2 {
		t.Fatalf("callbacks = %+v, want two", updates)
	}
	git.mu.Lock()
	ensureCalls := git.ensureCalls
	git.mu.Unlock()
	if ensureCalls != 0 {
		t.Fatalf("SSH prepared receive outside the durable transaction %d times", ensureCalls)
	}
	for _, update := range updates {
		if update.actor != "worker@tuxbox" || update.repo != "example" {
			t.Fatalf("callback lost authenticated identity: %+v", update)
		}
	}
	if updates[0].update.Ref != "refs/heads/main" || updates[0].update.OldSHA != shaA || updates[0].update.NewSHA != shaB {
		t.Fatalf("branch callback = %+v", updates[0])
	}
	if updates[1].update.Ref != "refs/tags/v1.0.0" || updates[1].update.NewSHA != shaC {
		t.Fatalf("tag callback = %+v", updates[1])
	}
}

func TestAuditMutationGateRejectsReceiveButAllowsUpload(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	git := &fakeGit{refs: map[string]string{"refs/heads/main": shaA}}
	server, err := New(Config{
		HostSigner: newSigner(t),
		Resolver: IdentityResolverFunc(func(fingerprint string) (string, bool, error) {
			return "agent@host", fingerprint == ssh.FingerprintSHA256(clientSigner.PublicKey()), nil
		}),
		Git: git, OnUpdate: noOpUpdateHandler(),
		MutationGate: func(context.Context) error { return errors.New("witness history unavailable") },
	})
	if err != nil {
		t.Fatal(err)
	}
	client, stop := startServer(t, server, clientSigner)
	defer stop()

	upload, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := upload.Run("git-upload-pack '/example.git'"); err != nil {
		t.Fatalf("read-only clone path was blocked: %v", err)
	}
	receive, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	receive.Stderr = &stderr
	if err := receive.Run("git-receive-pack '/example.git'"); err == nil {
		t.Fatal("receive-pack passed while audit integrity was unavailable")
	}
	if !strings.Contains(stderr.String(), "audit integrity is unavailable") {
		t.Fatalf("receive-pack error = %q", stderr.String())
	}
}

func TestPushActorIsBoundAtSSHHandshake(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	fingerprint := ssh.FingerprintSHA256(clientSigner.PublicKey())
	git := &fakeGit{
		refs:         map[string]string{"refs/heads/main": shaA},
		afterReceive: map[string]string{"refs/heads/main": shaB},
	}

	var identityMu sync.Mutex
	identity := "first-agent@tuxbox"
	resolver := IdentityResolverFunc(func(got string) (string, bool, error) {
		identityMu.Lock()
		defer identityMu.Unlock()
		return identity, got == fingerprint, nil
	})
	actors := make(chan string, 1)
	server, err := New(Config{
		HostSigner: newSigner(t), Resolver: resolver, Git: git,
		OnUpdate: UpdateHandlerFunc(func(_ context.Context, event gitcache.ReceiveEvent) error {
			actors <- event.Actor
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	client, stop := startServer(t, server, clientSigner)
	defer stop()
	identityMu.Lock()
	identity = "later-api-session@tuxbox"
	identityMu.Unlock()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Run("git-receive-pack '/example.git'"); err != nil {
		t.Fatal(err)
	}
	if actor := <-actors; actor != "first-agent@tuxbox" {
		t.Fatalf("push actor = %q, want identity authenticated at SSH handshake", actor)
	}
}

func TestStaleCacheWarningIsSentToGitClient(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	git := &fakeGit{stale: true}
	server := authorizedServer(t, clientSigner, git)
	client, stop := startServer(t, server, clientSigner)
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	session.Stderr = &stderr
	if err := session.Run("git-upload-pack '/example.git'"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("serving cached repository")) {
		t.Fatalf("missing stale warning: %q", stderr.String())
	}
}

func TestSlowHandshakeReleasesConnectionCapacity(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	fingerprint := ssh.FingerprintSHA256(clientSigner.PublicKey())
	server, err := New(Config{
		HostSigner: newSigner(t),
		Resolver: IdentityResolverFunc(func(got string) (string, bool, error) {
			return "agent@host", got == fingerprint, nil
		}),
		Git:              &fakeGit{},
		OnUpdate:         noOpUpdateHandler(),
		HandshakeTimeout: 500 * time.Millisecond,
		MaxConnections:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	slow, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return len(server.connections) == 1 })
	if client, err := ssh.Dial("tcp", listener.Addr().String(), clientConfig(clientSigner)); err == nil {
		_ = client.Close()
		t.Fatal("connection beyond configured cap unexpectedly authenticated")
	}
	waitFor(t, time.Second, func() bool { return len(server.connections) == 0 })
	client, err := ssh.Dial("tcp", listener.Addr().String(), clientConfig(clientSigner))
	if err != nil {
		t.Fatalf("capacity did not recover after handshake deadline: %v", err)
	}
	_ = client.Close()
	_ = slow.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticatedConnectionWithoutRequestReleasesCapacity(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	fingerprint := ssh.FingerprintSHA256(clientSigner.PublicKey())
	server, err := New(Config{
		HostSigner: newSigner(t),
		Resolver: IdentityResolverFunc(func(got string) (string, bool, error) {
			return "agent@host", got == fingerprint, nil
		}),
		Git: &fakeGit{}, OnUpdate: noOpUpdateHandler(),
		RequestTimeout: 100 * time.Millisecond, SessionTimeout: time.Second, MaxConnections: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	idle, err := ssh.Dial("tcp", listener.Addr().String(), clientConfig(clientSigner))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return len(server.connections) == 1 })
	waitFor(t, time.Second, func() bool { return len(server.connections) == 0 })
	client, err := ssh.Dial("tcp", listener.Addr().String(), clientConfig(clientSigner))
	if err != nil {
		t.Fatalf("capacity did not recover after authenticated request deadline: %v", err)
	}
	_ = client.Close()
	_ = idle.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSessionLimitRejectsAndRecovers(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	started := make(chan struct{})
	releaseServe := make(chan struct{})
	git := &fakeGit{serveStarted: started, serveRelease: releaseServe}
	fingerprint := ssh.FingerprintSHA256(clientSigner.PublicKey())
	server, err := New(Config{
		HostSigner: newSigner(t),
		Resolver: IdentityResolverFunc(func(got string) (string, bool, error) {
			return "agent@host", got == fingerprint, nil
		}),
		Git:                      git,
		OnUpdate:                 noOpUpdateHandler(),
		MaxSessions:              1,
		MaxSessionsPerConnection: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, stop := startServer(t, server, clientSigner)
	defer stop()
	first, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start("git-upload-pack '/example.git'"); err != nil {
		t.Fatal(err)
	}
	<-started
	if second, err := client.NewSession(); err == nil {
		_ = second.Close()
		t.Fatal("session beyond configured cap unexpectedly opened")
	}
	close(releaseServe)
	if err := first.Wait(); err != nil {
		t.Fatal(err)
	}
	third, err := client.NewSession()
	if err != nil {
		t.Fatalf("session capacity did not recover: %v", err)
	}
	if err := third.Run("git-upload-pack '/example.git'"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCapacityIsReleasedBeforeTerminalStatus(t *testing.T) {
	t.Parallel()
	server := &Server{git: &fakeGit{}}
	completed := false
	completeCalls := 0
	complete := sync.OnceFunc(func() {
		completed = true
		completeCalls++
	})
	channel := &completionObservingChannel{
		t: t,
		completed: func() bool {
			return completed
		},
	}
	requests := make(chan *ssh.Request, 1)
	requests <- &ssh.Request{
		Type:    "exec",
		Payload: ssh.Marshal(struct{ Command string }{Command: "git-upload-pack '/example.git'"}),
	}
	close(requests)
	server.handleSession(context.Background(), "agent@host", channel, requests, func() error { return nil }, complete)
	if !channel.sawExitStatus {
		t.Fatal("session did not send terminal status")
	}
	if completeCalls != 1 {
		t.Fatalf("session capacity release calls = %d, want one", completeCalls)
	}
}

func TestStartupReplayDelegatesDurableEvents(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	git := &fakeGit{}
	server := authorizedServer(t, clientSigner, git)
	if err := server.ReplayPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	if !git.replayCalled {
		t.Fatal("startup replay did not reach Git receive outbox")
	}
}

func authorizedServer(t *testing.T, clientSigner ssh.Signer, git gitcache.ServiceRunner) *Server {
	t.Helper()
	fingerprint := ssh.FingerprintSHA256(clientSigner.PublicKey())
	server, err := New(Config{
		HostSigner: newSigner(t),
		Resolver: IdentityResolverFunc(func(got string) (string, bool, error) {
			return "agent@host", got == fingerprint, nil
		}),
		Git:      git,
		OnUpdate: noOpUpdateHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func noOpUpdateHandler() UpdateHandler {
	return UpdateHandlerFunc(func(context.Context, gitcache.ReceiveEvent) error { return nil })
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

func startServer(t *testing.T, server *Server, signer ssh.Signer) (*ssh.Client, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	client, err := ssh.Dial("tcp", listener.Addr().String(), clientConfig(signer))
	if err != nil {
		cancel()
		_ = listener.Close()
		t.Fatal(err)
	}
	return client, func() {
		_ = client.Close()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("SSH server did not stop")
		}
	}
}

func clientConfig(signer ssh.Signer) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // Test-only listener and generated host key.
		Timeout:         2 * time.Second,
	}
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

type fakeGit struct {
	mu           sync.Mutex
	ensureCalls  int
	refs         map[string]string
	afterReceive map[string]string
	service      gitcache.Service
	protocol     string
	repo         string
	stale        bool
	serveStarted chan struct{}
	serveRelease chan struct{}
	serveOnce    sync.Once
	replayCalled bool
}

type completionObservingChannel struct {
	t             *testing.T
	completed     func() bool
	stderr        bytes.Buffer
	sawExitStatus bool
}

func (*completionObservingChannel) Read([]byte) (int, error) { return 0, io.EOF }

func (*completionObservingChannel) Write(body []byte) (int, error) { return len(body), nil }

func (*completionObservingChannel) Close() error { return nil }

func (*completionObservingChannel) CloseWrite() error { return nil }

func (channel *completionObservingChannel) SendRequest(name string, _ bool, _ []byte) (bool, error) {
	if name == "exit-status" {
		channel.sawExitStatus = true
		if !channel.completed() {
			channel.t.Error("session advertised terminal status before releasing capacity")
		}
	}
	return true, nil
}

func (channel *completionObservingChannel) Stderr() io.ReadWriter { return &channel.stderr }

func (f *fakeGit) Ensure(_ context.Context, repo string) (gitcache.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	if repo != "example" {
		return gitcache.Repository{}, fmt.Errorf("unknown repository %s", repo)
	}
	return gitcache.Repository{Path: "/data/git/example.git", Stale: f.stale}, nil
}

func (f *fakeGit) SnapshotRefs(context.Context, string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make(map[string]string, len(f.refs))
	for ref, sha := range f.refs {
		result[ref] = sha
	}
	return result, nil
}

func (f *fakeGit) Serve(_ context.Context, repo string, service gitcache.Service, protocol string, _ io.Reader, _, _ io.Writer) error {
	f.mu.Lock()
	f.repo = repo
	f.service = service
	f.protocol = protocol
	if service == gitcache.ReceivePack && f.afterReceive != nil {
		f.refs = make(map[string]string, len(f.afterReceive))
		for ref, sha := range f.afterReceive {
			f.refs[ref] = sha
		}
	}
	started, releaseServe := f.serveStarted, f.serveRelease
	f.mu.Unlock()
	if service == gitcache.UploadPack && started != nil {
		f.serveOnce.Do(func() {
			close(started)
			<-releaseServe
		})
	}
	return nil
}

func (f *fakeGit) Receive(ctx context.Context, repo, actor, protocol string, _ io.Reader, _, _ io.Writer, handler gitcache.ReceiveHandler) error {
	f.mu.Lock()
	f.repo = repo
	f.service = gitcache.ReceivePack
	f.protocol = protocol
	before := cloneRefs(f.refs)
	if f.afterReceive != nil {
		f.refs = cloneRefs(f.afterReceive)
	}
	after := cloneRefs(f.refs)
	f.mu.Unlock()
	for index, update := range gitcache.DiffRefs(before, after) {
		if err := handler.HandleReceive(ctx, gitcache.ReceiveEvent{ID: fmt.Sprintf("fake:%d", index), Actor: actor, Repo: repo, Update: update}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeGit) ReplayPending(context.Context, gitcache.ReceiveHandler) error {
	f.mu.Lock()
	f.replayCalled = true
	f.mu.Unlock()
	return nil
}

func cloneRefs(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for ref, sha := range source {
		result[ref] = sha
	}
	return result
}
