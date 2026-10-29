//go:build linux

package gitssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/testtimeout"
)

// Real Git leaves the helper's pipe stdin open while awaiting the initial
// remote advertisement. A string reader cannot reproduce that half-close.
func TestRealGitReturnsWhenRemoteExitsBeforeReadingStdin(t *testing.T) {
	signer, _ := testKey(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	var connections sync.Map
	defer func() {
		listener.Close()
		connections.Range(func(k, v any) bool { _ = k.(net.Conn).Close(); return true })
		workers.Wait()
	}()
	requestsSeen := make(chan string, 2)
	workers.Go(func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(raw, true)
			workers.Go(func() {
				defer raw.Close()
				defer connections.Delete(raw)
				cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
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
					request, ok := <-requests
					if !ok {
						channel.Close()
						return
					}
					var command struct{ Command string }
					if request.Type != "exec" || ssh.Unmarshal(request.Payload, &command) != nil {
						request.Reply(false, nil)
						channel.Close()
						return
					}
					request.Reply(true, nil)
					requestsSeen <- command.Command
					// Never read the channel's stdin, including after the exit status.
					_, _ = io.WriteString(channel.Stderr(), "fatal: repository not found\n")
					_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{128}))
					channel.Close()
				}
			})
		}
	})
	config := configFile(t, listener.Addr().String(), signer.PublicKey())
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append([]byte("Host *\n HostName 127.0.0.1\n"), body...), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "git-ssh-fixture")
	if err := os.Symlink(executable, helper); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GORACE=atexit_sleep_ms=0"}
	for _, args := range [][]string{{"init", "--quiet", "-b", "main"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "--quiet", "-m", "seed"}} {
		command := exec.CommandContext(t.Context(), "git", args...)
		command.Dir = work
		command.Env = env
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, out)
		}
	}
	for _, test := range []struct {
		service string
		args    []string
	}{
		{"git-upload-pack", []string{"ls-remote", "git@fixture:/fixture"}},
		{"git-receive-pack", []string{"push", "git@fixture:/fixture", "HEAD:refs/heads/refused"}},
	} {
		t.Run(test.service, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(60*time.Second))
			defer cancel()
			command := exec.CommandContext(ctx, "git", test.args...)
			command.Dir = work
			command.Env = append(append([]string{}, env...), "GIT_SSH="+helper, "GIT_SSH_VARIANT=simple", ConfigEnvironment+"="+config, "OBERTH_GITSSH_FIXTURE_PROCESS=1")
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
			command.WaitDelay = time.Second
			out, err := command.CombinedOutput()
			var exit *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 128 {
				t.Fatalf("real Git did not return remote 128: %v %s", err, out)
			}
			select {
			case command := <-requestsSeen:
				if command != fmt.Sprintf("%s '/fixture'", test.service) {
					t.Fatalf("wrong service %q", command)
				}
			default:
				t.Fatal("Git failed before reaching the SSH service")
			}
			if !strings.Contains(string(out), "repository not found") {
				t.Fatalf("forge refusal missing: %s", out)
			}
		})
	}
}
