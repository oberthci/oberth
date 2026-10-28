//go:build staticgit_qualification && linux

package gitcache

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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// This qualification is compiled explicitly and run in the scratch runtime.
// It must use the actual bundled Git and Oberth alias, with no host tool mount.
func TestBundledGitNativeSSHCloneFetchRefusalAndCancel(t *testing.T) {
	const helper = "/usr/local/bin/oberth-git-ssh"
	if target, err := os.Readlink(helper); err != nil || target != "oberth" {
		t.Fatalf("native runtime alias missing: %q %v", target, err)
	}
	key := func() (ssh.Signer, []byte) {
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
	host, _ := key()
	wrong, _ := key()
	identity, private := key()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	// Close accepted connections before joining even if an assertion fails.
	var connections sync.Map
	defer func() {
		listener.Close()
		connections.Range(func(k, v any) bool { _ = k.(net.Conn).Close(); return true })
		workers.Wait()
	}()

	var auth atomic.Int32
	var earlyExit atomic.Bool
	var stall atomic.Bool
	stalled := make(chan struct{}, 1)
	repository := newTestRepository(t)
	workers.Go(func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(raw, true)
			workers.Go(func() {
				defer connections.Delete(raw)
				defer raw.Close()
				cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, pub ssh.PublicKey) (*ssh.Permissions, error) {
					auth.Add(1)
					if string(pub.Marshal()) != string(identity.PublicKey().Marshal()) {
						return nil, fmt.Errorf("wrong fixture identity")
					}
					return nil, nil
				}}
				cfg.AddHostKey(host)
				connection, channels, requests, err := ssh.NewServerConn(raw, cfg)
				if err != nil {
					return
				}
				defer connection.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						_ = incoming.Reject(ssh.UnknownChannelType, "refused")
						continue
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					request, ok := <-requests
					if !ok {
						channel.Close()
						return
					}
					var command struct{ Command string }
					if request.Type != "exec" || ssh.Unmarshal(request.Payload, &command) != nil || (command.Command != "git-upload-pack '/fixture'" && !(earlyExit.Load() && command.Command == "git-receive-pack '/fixture'")) {
						request.Reply(false, nil)
						channel.Close()
						return
					}
					request.Reply(true, nil)
					if earlyExit.Load() {
						_, _ = io.WriteString(channel.Stderr(), "fatal: repository not found\n")
						_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{128}))
						channel.Close()
						continue
					}
					if stall.Load() {
						select {
						case stalled <- struct{}{}:
						default:
						}
						_, _ = io.Copy(io.Discard, channel)
						channel.Close()
						continue
					}
					cmd := exec.Command("/usr/bin/git", "upload-pack", repository.upstream)
					cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
					input, err := cmd.StdinPipe()
					if err != nil {
						channel.Close()
						return
					}
					copied := make(chan struct{})
					go func() { defer close(copied); _, _ = io.Copy(input, channel); _ = input.Close() }()
					cmd.Stdout = channel
					cmd.Stderr = channel.Stderr()
					err = cmd.Run()
					status := uint32(0)
					if err != nil {
						status = 1
					}
					channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{status}))
					channel.Close()
					<-copied
				}
			})
		}
	})
	root := t.TempDir()
	config := filepath.Join(root, "config")
	hosts := filepath.Join(root, "known_hosts")
	identityFile := filepath.Join(root, "identity")
	if err := os.WriteFile(identityFile, private, 0600); err != nil {
		t.Fatal(err)
	}
	pin := func(pub ssh.PublicKey) {
		t.Helper()
		if err := os.WriteFile(hosts, []byte(knownhosts.Line([]string{listener.Addr().String()}, pub)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pin(host.PublicKey())
	body := fmt.Sprintf("Host *\n IdentityFile %q\n IdentitiesOnly yes\n BatchMode yes\n StrictHostKeyChecking yes\n GlobalKnownHostsFile /dev/null\n UserKnownHostsFile %q\n", identityFile, hosts)
	if err := os.WriteFile(config, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cache, err := New(Config{Root: filepath.Join(root, "cache"), Env: map[string]string{"GIT_SSH": helper, "GIT_SSH_VARIANT": "simple", "OBERTH_GIT_SSH_CONFIG": config}, Upstream: func(string) (string, error) { return "unused", nil }})
	if err != nil {
		t.Fatal(err)
	}
	url := "ssh://git@" + listener.Addr().String() + "/fixture"
	clone := filepath.Join(root, "clone")
	if err := cache.run(t.Context(), commandSpec{args: []string{"clone", url, clone}}); err != nil {
		t.Fatal(err)
	}
	want := repository.commit(t, "native SSH fetch", "new remote value\n")
	if err := cache.run(t.Context(), commandSpec{dir: clone, args: []string{"fetch", "origin"}}); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, clone, "rev-parse", "origin/main"); got != want {
		t.Fatalf("fetch=%s want %s", got, want)
	}
	before := auth.Load()
	pin(wrong.PublicKey())
	if err := cache.run(t.Context(), commandSpec{args: []string{"ls-remote", url}}); err == nil {
		t.Fatal("host-key mismatch accepted")
	}
	if auth.Load() != before {
		t.Fatal("authentication attempted before host-key check")
	}
	pin(host.PublicKey())
	earlyExit.Store(true)
	for _, args := range [][]string{{"ls-remote", url}, {"-C", clone, "push", url, "HEAD:refs/heads/refused"}} {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		err := cache.run(ctx, commandSpec{args: args})
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 128 {
			t.Fatalf("early remote exit %v: %v", args, err)
		}
	}
	earlyExit.Store(false)
	stall.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cache.run(ctx, commandSpec{args: []string{"ls-remote", url}}) }()
	select {
	case <-stalled:
	case err := <-done:
		t.Fatalf("native Git exited before stalled service: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("native helper did not reach stalled service")
	}
	// Capture the actual Git/helper PIDs while alive, before cancellation.
	var pids []int
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		data, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if strings.Contains(string(data), url) || strings.Contains(string(data), "git-upload-pack '/fixture'") {
			pids = append(pids, pid)
		}
	}
	if len(pids) < 2 {
		t.Fatalf("expected Git plus native helper, got %v", pids)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled fetch succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceled Git/helper not reaped")
	}
	for _, pid := range pids {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
			t.Fatalf("Git/helper PID %d remains (including zombie): %v", pid, err)
		}
	}
	listener.Close()
}
