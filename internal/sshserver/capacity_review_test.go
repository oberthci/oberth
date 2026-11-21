package sshserver

import (
	"context"
	"errors"
	"k8s.io/utils/clock"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
)

type queuedListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (l *queuedListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *queuedListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (*queuedListener) Addr() net.Addr { return &net.UnixAddr{Name: "memory", Net: "unix"} }

type watchedConn struct {
	net.Conn
	deadline, closed chan struct{}
	once             sync.Once
}

func (c *watchedConn) SetDeadline(d time.Time) error { close(c.deadline); return c.Conn.SetDeadline(d) }
func (c *watchedConn) Close() error                  { c.once.Do(func() { close(c.closed) }); return c.Conn.Close() }
func TestIncomingConnectionsAccept64Refuse65(t *testing.T) {
	s, err := New(Config{Signer: signer(t), Lookup: func(context.Context, ssh.PublicKey) (Identity, error) { return Identity{}, nil }, Run: func(context.Context, Session) error { return nil }, Now: (clock.RealClock{}).Now})
	if err != nil {
		t.Fatal(err)
	}
	listener := &queuedListener{connections: make(chan net.Conn), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener); close(done) }()
	defer func() { cancel(); <-done }()
	for i := range 65 {
		left, right := sshtest.Pipe()
		t.Cleanup(func() { _ = left.Close() })
		conn := &watchedConn{Conn: addressedConn{Conn: right, address: &net.TCPAddr{IP: net.IPv4(192, 0, 2, byte(i+1)), Port: 1234}}, deadline: make(chan struct{}), closed: make(chan struct{})}
		listener.connections <- conn
		if i < 64 {
			select {
			case <-conn.deadline:
			case <-conn.closed:
				t.Fatal("connection refused below capacity", i)
			}
		} else {
			select {
			case <-conn.closed:
			case <-conn.deadline:
				t.Fatal("65th connection accepted")
			}
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s.config.Idle != 30*time.Second || s.config.Handshake != 5*time.Second {
		t.Fatal("default deadlines changed")
	}
}
func TestUnauthenticatedHandshakeHonorsDeadline(t *testing.T) {
	s, err := New(Config{Signer: signer(t), Lookup: func(context.Context, ssh.PublicKey) (Identity, error) {
		t.Error("silent peer authenticated")
		return Identity{}, nil
	}, Run: func(context.Context, Session) error { return nil }, Now: (clock.RealClock{}).Now, Handshake: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	left, right := sshtest.Pipe()
	defer func() { _ = left.Close() }()
	done := make(chan error, 1)
	go func() { done <- s.Handle(t.Context(), right) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("silent handshake accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("handshake deadline not enforced")
	}
}
