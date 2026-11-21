package githubfake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ServeSSH serves real Git subprocesses and waits for them on cancellation.
func (f *Fake) ServeSSH(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	defer cancel()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		workers.Add(1)
		go func() { defer workers.Done(); f.serveConnection(ctx, conn) }()
	}
}
func (f *Fake) sshConfig() *ssh.ServerConfig {
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			f.record("auth", "*", false)
			wire := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			if meta.User() != "git" || f.takeFault("auth", "*") != nil || !f.access(wire, "").read {
				return nil, errors.New("permission denied")
			}
			return &ssh.Permissions{Extensions: map[string]string{"public_key": wire}}, nil
		},
		AuthLogCallback: func(meta ssh.ConnMetadata, method string, err error) {
			if err != nil && (meta.User() != "git" || method != "none" && method != "publickey") {
				f.record("auth-"+method, "*", true)
			}
		},
	}
	for _, signer := range f.signers {
		config.AddHostKey(signer)
	}
	return config
}
func (f *Fake) serveConnection(ctx context.Context, raw net.Conn) {
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	_ = raw.SetDeadline(f.now().Add(10 * time.Second))
	conn, channels, requests, err := ssh.NewServerConn(raw, f.sshConfig())
	if err != nil {
		return
	}
	defer conn.Close()
	_ = raw.SetDeadline(time.Time{})
	go func() {
		for req := range requests {
			known := req.Type == "no-more-sessions@openssh.com"
			f.record("ssh-global", req.Type, !known)
			_ = req.Reply(known, nil)
		}
	}()
	var sessions sync.WaitGroup
	defer sessions.Wait()
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			f.record("ssh-channel", incoming.ChannelType(), true)
			_ = incoming.Reject(ssh.UnknownChannelType, "UNEXPECTED channel")
			continue
		}
		ch, requests, err := incoming.Accept()
		if err != nil {
			continue
		}
		sessions.Add(1)
		go func() { defer sessions.Done(); f.serveSession(ctx, conn, ch, requests) }()
	}
}

type access struct{ read, write, exists bool }

// An empty name asks whether the key can authenticate to any repository.
func (f *Fake) access(wire, name string) access {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := access{exists: f.repos[name] != nil}
	a.read = slices.Contains(f.authorized, wire)
	a.write = a.read
	for repoName, repo := range f.repos {
		if name != "" && repoName != name {
			continue
		}
		for _, allowed := range repo {
			if allowed.Key == wire {
				a.read, a.write = true, a.write || !allowed.ReadOnly
			}
		}
	}
	return a
}

var gitExec = regexp.MustCompile(`^(git-upload-pack|git-receive-pack) '([^']+)'$`)

func (f *Fake) sessionEnv(req *ssh.Request) string {
	var v struct{ Name, Value string }
	err := ssh.Unmarshal(req.Payload, &v)
	ok := err == nil && v.Name == "GIT_PROTOCOL" && (v.Value == "version=1" || v.Value == "version=2")
	harmless := v.Name == "LANG" || strings.HasPrefix(v.Name, "LC_") || v.Name == "COLORTERM" || v.Name == "NO_COLOR"
	if !ok && (err != nil || !harmless) {
		f.record("ssh-env", v.Name, true)
	}
	_ = req.Reply(ok, nil)
	if ok {
		return v.Value
	}
	return ""
}
func (f *Fake) sessionCommand(req *ssh.Request) (string, string) {
	var v struct{ Command string }
	if req.Type != "exec" || ssh.Unmarshal(req.Payload, &v) != nil {
		f.record("ssh-request", req.Type, true)
		return "", ""
	}
	match := gitExec.FindStringSubmatch(v.Command)
	if match == nil {
		f.record("ssh-exec", v.Command, true)
		return "", ""
	}
	name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(match[2], "~"), "/"), ".git")
	if !validRepo(name) {
		f.record(match[1], match[2], true)
		return "", ""
	}
	return match[1], strings.ToLower(name)
}
func (f *Fake) serveSession(ctx context.Context, conn *ssh.ServerConn, ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()
	protocol := ""
	for req := range requests {
		if req.Type == "env" {
			if value := f.sessionEnv(req); value != "" {
				protocol = value
			}
			continue
		}
		operation, name := f.sessionCommand(req)
		_ = req.Reply(operation != "", nil)
		if operation == "" {
			return
		}
		f.record(operation, name, false)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			for extra := range requests {
				f.record("ssh-request", extra.Type, true)
				_ = extra.Reply(false, nil)
			}
			cancel()
		}()
		a := f.access(conn.Permissions.Extensions["public_key"], name)
		switch {
		case !a.exists || !a.read:
			sshFailure(ch, "repository not found", 128)
		case operation == "git-receive-pack" && !a.write:
			sshFailure(ch, "permission denied", 128)
		default:
			if !f.sshFault(ctx, conn, ch, f.takeFault(operation, name)) {
				f.runGit(ctx, ch, []string{strings.TrimPrefix(operation, "git-"), f.repoPath(name)}, protocol)
			}
		}
		return
	}
}
func (f *Fake) sshFault(ctx context.Context, conn *ssh.ServerConn, ch ssh.Channel, v *Fault) bool {
	if v == nil {
		return false
	}
	switch v.Kind {
	case "slow":
		return !f.delay(ctx, time.Duration(v.DelayMS)*time.Millisecond)
	case "drop":
		_ = conn.Close()
	default:
		message := map[string]string{"not_found": "repository not found", "forbidden": "permission denied", "server_error": "internal server error", "archived": "This repository was archived so it is read-only", "protected": "protected branch update refused"}[v.Kind]
		sshFailure(ch, message, 128)
	}
	return true
}
func (f *Fake) runGit(ctx context.Context, ch ssh.Channel, args []string, protocol string) {
	command := f.git(ctx, args...)
	if protocol != "" {
		command.Env = append(command.Env, "GIT_PROTOCOL="+protocol)
	}
	command.Stdout, command.Stderr = ch, ch.Stderr()
	input, err := command.StdinPipe()
	if err != nil {
		sshFailure(ch, "service unavailable", 128)
		return
	}
	if err := command.Start(); err != nil {
		_ = input.Close()
		sshFailure(ch, "service unavailable", 128)
		return
	}
	go func() { _, _ = io.Copy(input, ch); _ = input.Close() }()
	_ = command.Wait()
	code := command.ProcessState.ExitCode()
	if code < 0 {
		code = 128
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{uint32(code)}))
}
func sshFailure(ch ssh.Channel, message string, code uint32) {
	_, _ = fmt.Fprintln(ch.Stderr(), message)
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{code}))
}
