package sshserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
)

func checkTransportDeadline(t *testing.T) {
	f := gittest.New(t)
	cache := f.Cache(t, nil)
	host, key := signer(t), signer(t)
	left, right := sshtest.Pipe()
	observed := &deadlineConn{Conn: right}
	listener := sshtest.Listener(observed)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	commandDeadline := make(chan time.Time, 1)
	server, err := New(Config{Signer: host, Now: func() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }, Total: time.Hour,
		Lookup: func(context.Context, ssh.PublicKey) (Identity, error) {
			return Identity{Actor: "caller", Fingerprint: "key", CanAccess: func(repo string) bool { return repo == gittest.Repository }}, nil
		},
		Run: func(ctx context.Context, s Session) error {
			deadline, _ := ctx.Deadline()
			commandDeadline <- deadline
			err := cache.Upload(ctx, s.Command.Repository, gitcache.ServiceIO{Input: s.Input, Output: s.Output, Begin: s.Begin, End: s.End})
			return err
		}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	native, channels, requests, err := ssh.NewClientConn(left, "local:22", &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(native, channels, requests)
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	session.Stdin = strings.NewReader("0000")
	output, err := session.Output("git-upload-pack '/origin/team/widget.git'")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "refs/heads/main") {
		t.Fatal(string(output))
	}
	deadline := <-commandDeadline
	if got := observed.last(); deadline.IsZero() || !got.Equal(deadline) {
		t.Fatalf("connection deadline %s; command deadline %s", got, deadline)
	}
	_ = client.Close()
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type deadlineConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
}

func (c *deadlineConn) SetDeadline(d time.Time) error {
	c.mu.Lock()
	c.deadline = d
	c.mu.Unlock()
	return c.Conn.SetDeadline(d)
}
func (c *deadlineConn) last() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.deadline }

func TestIdleTimeoutAbortsBlockedTransport(t *testing.T) {
	now := time.Unix(0, 0)
	ch := &stalledChannel{started: make(chan struct{}), aborted: make(chan struct{})}
	server := &Server{config: Config{Idle: time.Hour, Now: func() time.Time { return now }, Run: func(ctx context.Context, s Session) error {
		s.Begin()
		defer s.End()
		done := make(chan error, 1)
		go func() { _, err := s.Output.Write([]byte("stalled")); done <- err }()
		<-ch.started
		now = now.Add(time.Hour)
		s.Output.(*idleChannel).fire()
		select {
		case <-ch.aborted:
		default:
			t.Error("idle timeout did not abort the underlying connection")
			close(ch.aborted)
		}
		if err := <-done; !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("blocked writer: %v", err)
		}
		if !errors.Is(context.Cause(ctx), context.Canceled) {
			t.Error("idle timeout did not cancel the command")
		}
		return nil
	}}}
	server.execute(t.Context(), peer{abort: func() { close(ch.aborted) }}, Command{Service: "git-upload-pack"}, ch)
}

// A channel close can itself wait behind a blocked transport write. Only
// closing the underlying connection releases this writer.
type stalledChannel struct {
	ssh.Channel
	started, aborted chan struct{}
}

func (c *stalledChannel) Write([]byte) (int, error) {
	close(c.started)
	<-c.aborted
	return 0, io.ErrClosedPipe
}
func (c *stalledChannel) Close() error                                   { return nil }
func (c *stalledChannel) Stderr() io.ReadWriter                          { return &bytes.Buffer{} }
func (c *stalledChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
