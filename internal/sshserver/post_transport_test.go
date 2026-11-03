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
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestReceiveFinalizationOutlivesTransportIdleDeadline(t *testing.T) {
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
	var git *phaseObservedGit
	cache, err := gitcache.New(gitcache.Config{Root: filepath.Join(root, "cache"), AllowLocalUpstreams: true, PreFinalizeGate: func(ctx context.Context) error {
		stream := git.stream
		stream.mu.Lock()
		stopped := stream.stopped
		stream.mu.Unlock()
		if !stopped {
			return errors.New("transport idle timer survived Git exit")
		}
		// Invoke expiry checking during finalization without depending on host
		// scheduling or making a real Git transport fit a 100 ms deadline.
		stream.check()
		return ctx.Err()
	}, Upstream: func(string) (string, error) { return upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	git = &phaseObservedGit{Cache: cache}
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
	server, err := New(Config{HostSigner: newSigner(t), Resolver: IdentityResolverFunc(func(string) (string, bool, error) { return "actor", true, nil }), Git: git, OnUpdate: noOpUpdateHandler()})
	if err != nil {
		t.Fatal(err)
	}
	client, stop := startServer(t, server, signer)
	defer stop()
	_, port, err := net.SplitHostPort(client.RemoteAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	command := exec.CommandContext(ctx, "git", "-C", work, "-c", "core.sshCommand=ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "+quote(keyPath), "push", fmt.Sprintf("ssh://git@127.0.0.1:%s/example", port), "HEAD:refs/heads/feature")
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("server finalization was cut off by stream idle timer: %v: %s", err, output)
	}
}

type phaseObservedGit struct {
	*gitcache.Cache
	stream *idleChannel
}

func (g *phaseObservedGit) Receive(ctx context.Context, input, actor, protocol string, stdin io.Reader, stdout, stderr io.Writer, handler gitcache.ReceiveHandler) error {
	g.stream = stdin.(*idleChannel)
	return g.Cache.Receive(ctx, input, actor, protocol, stdin, stdout, stderr, handler)
}
