//go:build base_image_qualification

package sshserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/model"
	"golang.org/x/crypto/ssh"
	"k8s.io/utils/clock"
)

type baseRuntime struct {
	environment                              *reviewEnv
	url, configuration, shim, pins, identity string
	sessions, gates                          atomic.Int64
}

func TestBaseImageCloneFetchPushAndPinnedHost(t *testing.T) {
	r := newBaseRuntime(t)
	f := r.environment.f
	checkout := filepath.Join(t.TempDir(), "client")
	baseGit(t, r, f.Root, "clone", r.url, checkout)
	if got := baseGit(t, r, checkout, "rev-parse", "HEAD"); got != f.Commit {
		t.Fatal(got)
	}
	f.Write(t, "next.txt", []byte("next\n"))
	next := f.CommitFile(t, "upstream change")
	f.Git(t, f.Store(), "fetch", f.Work, "refs/heads/main:refs/heads/main")
	baseGit(t, r, checkout, "fetch", "origin", "main")
	if got := baseGit(t, r, checkout, "rev-parse", "FETCH_HEAD"); got != next {
		t.Fatal(got)
	}
	baseGit(t, r, checkout, "config", "user.name", "Oberth")
	baseGit(t, r, checkout, "config", "user.email", "ci@oberth.ci")
	if err := os.WriteFile(filepath.Join(checkout, "pushed.txt"), []byte("pushed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	baseGit(t, r, checkout, "add", ".")
	baseGit(t, r, checkout, "commit", "-m", "qualified push")
	commit := baseGit(t, r, checkout, "rev-parse", "HEAD")
	baseGit(t, r, checkout, "push", "origin", "HEAD:refs/heads/base-qualified")
	if r.gates.Load() != 1 || r.environment.ref(t, "refs/heads/base-qualified") != commit {
		t.Fatal("native receive hook did not admit the exact pushed commit")
	}
	before := r.sessions.Load()
	known, err := os.ReadFile(r.pins)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := strings.Fields(string(known))[0]
	if err := os.WriteFile(r.pins, []byte(endpoint+" "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer(t).PublicKey())))+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := baseCommand(r, checkout, "fetch", "origin").CombinedOutput(); err == nil {
		t.Fatal("unpinned host accepted")
	}
	if r.sessions.Load() != before {
		t.Fatal("host-key refusal reached a Git service")
	}
}

func TestBaseImageServerCloneFetchPublish(t *testing.T) {
	r := newBaseRuntime(t)
	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := gitcache.New(t.Context(), gitcache.Config{Root: filepath.Join(root, "cache"), Scratch: filepath.Join(root, "scratch"), Binary: "/usr/bin/git", HookBinary: binary,
		LiveOwners: func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil },
		Upstream: func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{RepositoryID: 1, URL: r.url, SSHBinary: r.shim, SSHConfig: r.configuration, DefaultBranch: "refs/heads/main"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	if err = mirror.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal("server clone", err)
	}
	gittest.ExpireRefresh(t, filepath.Join(root, "cache", "origin", "team", "widget.git"))
	if err = mirror.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("server fetch", err)
	}
	object, previous := r.environment.f.Commit, ""
	baseGit(t, r, filepath.Join(root, "cache", "origin", "team", "widget.git"), "update-ref", "refs/heads/server-qualified", object)
	request := model.Publication{ID: "base-qualified", Repo: gittest.Repository, Ref: "refs/heads/server-qualified", ObjectSHA: object, CommitSHA: &object, PreviousSHA: &previous, Actor: "operator"}
	if _, err = mirror.Publish(t.Context(), request); err != nil {
		t.Fatal("server push", err)
	}
	if r.gates.Load() != 1 || r.environment.ref(t, request.Ref) != object {
		t.Fatal("server publication missed the native receive hook or exact ref")
	}
}

func newBaseRuntime(t *testing.T) *baseRuntime {
	t.Helper()
	baseImageEnvironment(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := &baseRuntime{}
	r.environment = newReviewEnvConfigured(t, func(config *gitcache.Config) { config.HookBinary = binary })
	r.environment.gate = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		r.gates.Add(1)
		return nil
	}
	directory := t.TempDir()
	r.shim = filepath.Join(directory, "oberth-git-ssh")
	if err = os.Symlink(binary, r.shim); err != nil {
		t.Fatal(err)
	}
	signerKey, identity := baseClientIdentity(t, directory)
	r.identity = identity
	host := signer(t)
	address := r.serve(t, host, signerKey)
	r.configureTrust(t, directory, address, host)
	return r
}

func baseImageEnvironment(t *testing.T) {
	t.Helper()
	baseRuntimeFiles(t)
	for _, path := range []string{"/bin/sh", "/usr/bin/ssh", "/bin/ssh", "/usr/bin/python3"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("qualification image contains a runner tool", path, err)
		}
	}
	for key, value := range map[string]string{"PATH": "/usr/local/bin:/usr/bin", "HOME": "/data", "TMPDIR": "/tmp"} {
		if os.Getenv(key) != value {
			t.Fatal("packaging environment differs", key)
		}
	}
}

func baseRuntimeFiles(t *testing.T) {
	t.Helper()
	bundle, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
	if err != nil {
		t.Fatal(err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(bundle) {
		t.Fatal("runtime trust bundle has no certificates")
	}
	directory, err := os.MkdirTemp("/data", "base-qualification-")
	if err != nil {
		t.Fatal("runtime HOME is not writable", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	temporary, err := os.Stat("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !temporary.IsDir() || temporary.Mode()&os.ModeSticky == 0 {
		t.Fatal("runtime temporary directory lacks its sticky bit")
	}
}

func baseClientIdentity(t *testing.T, directory string) (ssh.PublicKey, string) {
	t.Helper()
	client, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signerKey, err := ssh.NewPublicKey(client)
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(directory, "identity")
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(identity, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return signerKey, identity
}

func (r *baseRuntime) configureTrust(t *testing.T, directory, address string, host ssh.Signer) {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	r.pins = filepath.Join(directory, "known_hosts")
	if err = os.WriteFile(r.pins, append([]byte("[127.0.0.1]:"+port+" "), ssh.MarshalAuthorizedKey(host.PublicKey())...), 0600); err != nil {
		t.Fatal(err)
	}
	r.configuration = filepath.Join(directory, "ssh-config")
	configuration := fmt.Sprintf("Host 127.0.0.1\n HostName 127.0.0.1\n User git\n Port %s\n IdentitiesOnly yes\n BatchMode yes\n StrictHostKeyChecking yes\n GlobalKnownHostsFile /dev/null\n IdentityFile %s\n UserKnownHostsFile %s\n", port, r.identity, r.pins)
	if err = os.WriteFile(r.configuration, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	r.url = "ssh://git@" + address + "/" + gittest.Repository + ".git"
}

func (r *baseRuntime) serve(t *testing.T, host ssh.Signer, client ssh.PublicKey) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Signer: host, Now: clock.RealClock{}.Now, Lookup: func(_ context.Context, key ssh.PublicKey) (Identity, error) {
		if string(key.Marshal()) != string(client.Marshal()) {
			return Identity{}, &UnregisteredError{}
		}
		return Identity{Actor: "operator", Fingerprint: "qualified", CanAccess: func(repo string) bool { return repo == gittest.Repository }}, nil
	}, Run: r.run})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	t.Cleanup(func() { cancel(); <-done })
	return listener.Addr().String()
}

func (r *baseRuntime) run(ctx context.Context, s Session) error {
	r.sessions.Add(1)
	c := r.environment.c
	if s.Command.Service == "git-upload-pack" {
		return c.Upload(ctx, s.Command.Repository, gitcache.ServiceIO{Input: s.Input, Output: s.Output, Begin: s.Begin, End: s.End})
	}
	return c.Receive(ctx, gitcache.ReceiveRequest{Repository: s.Command.Repository, Actor: s.Identity.Actor, Fingerprint: s.Identity.Fingerprint, AllowDefault: true, AlwaysRun: true,
		Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: r.environment.f.Commit}, Input: s.Input, Output: s.Output, Diagnostics: s.Diagnostics, Begin: s.Begin, End: s.End,
		Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }})
}

func baseCommand(r *baseRuntime, directory string, args ...string) *exec.Cmd {
	command := exec.Command("/usr/bin/git", args...)
	command.Dir = directory
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin", "HOME=/data", "TMPDIR=/tmp", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_SSH=" + r.shim, "GIT_SSH_VARIANT=simple", gitssh.ConfigEnvironment + "=" + r.configuration, "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z"}
	return command
}

func baseGit(t *testing.T, r *baseRuntime, directory string, args ...string) string {
	t.Helper()
	output, err := baseCommand(r, directory, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("Git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
