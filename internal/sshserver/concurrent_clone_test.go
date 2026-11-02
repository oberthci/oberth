package sshserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"github.com/oberthci/oberth/internal/gitcache"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStalledSSHUploadAllowsAnotherClone(t *testing.T) {
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	work := filepath.Join(root, "work")
	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", args[0], err, out)
		}
	}
	gitRun("init", "--bare", "--initial-branch=main", upstream)
	gitRun("init", "--initial-branch=main", work)
	gitRun("-C", work, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial")
	gitRun("-C", work, "push", upstream, "main")
	cache, err := gitcache.New(gitcache.Config{Root: filepath.Join(root, "cache"), AllowLocalUpstreams: true, Upstream: func(string) (string, error) { return upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "identity")
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{HostSigner: newSigner(t), Resolver: IdentityResolverFunc(func(string) (string, bool, error) { return "actor", true, nil }), Git: cache, OnUpdate: noOpUpdateHandler()})
	if err != nil {
		t.Fatal(err)
	}
	client, stop := startServer(t, server, signer)
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	advertised := make(chan struct{})
	session.Stdout = &cloneAdvertisement{ready: advertised}
	if err = session.Start("git-upload-pack 'example'"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-advertised:
	case <-time.After(5 * time.Second):
		t.Fatal("upload never advertised")
	}
	_, port, err := net.SplitHostPort(client.RemoteAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	command := exec.CommandContext(ctx, "git", "-c", "core.sshCommand=ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "+quote(keyPath), "clone", "--quiet", fmt.Sprintf("ssh://git@127.0.0.1:%s/example", port), filepath.Join(root, "clone"))
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("second clone blocked by stalled upload: %v: %s", err, output)
	}
}

type cloneAdvertisement struct {
	ready chan struct{}
	once  sync.Once
}

func (w *cloneAdvertisement) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return io.Discard.Write(p)
}
