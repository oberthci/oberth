package sshserver

import (
	"context"
	"net"
	"testing"

	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
	"k8s.io/utils/clock"
)

type addressedConn struct {
	net.Conn
	address net.Addr
}

func (c addressedConn) RemoteAddr() net.Addr { return c.address }

func TestSilentPeerCannotHoldOtherPeersSlots(t *testing.T) {
	s, err := New(Config{Signer: signer(t), Lookup: func(context.Context, ssh.PublicKey) (Identity, error) { return Identity{}, nil }, Run: func(context.Context, Session) error { return nil }, Now: (clock.RealClock{}).Now})
	if err != nil {
		t.Fatal(err)
	}
	listener := &queuedListener{connections: make(chan net.Conn), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener) }()
	defer func() { cancel(); <-done }()
	for i := range 34 {
		left, right := sshtest.Pipe()
		t.Cleanup(func() { _ = left.Close() })
		ip := net.IPv4(192, 0, 2, 1)
		if i == 33 {
			ip = net.IPv4(192, 0, 2, 2)
		}
		c := &watchedConn{Conn: addressedConn{Conn: right, address: &net.TCPAddr{IP: ip, Port: 1000 + i}}, deadline: make(chan struct{}), closed: make(chan struct{})}
		listener.connections <- c
		select {
		case <-c.deadline:
			if i == 32 {
				t.Fatal("33rd unauthenticated connection from same peer accepted")
			}
		case <-c.closed:
			if i != 32 {
				t.Fatal("other peer or in-budget connection refused", i)
			}
		}
	}
}

func TestSuccessfulAuthenticationReleasesPeerBudget(t *testing.T) {
	host, key := signer(t), signer(t)
	s, err := New(Config{Signer: host, Now: (clock.RealClock{}).Now, UnauthenticatedPerPeer: 1, Lookup: func(context.Context, ssh.PublicKey) (Identity, error) {
		return Identity{Actor: "agent", CanAccess: func(string) bool { return true }}, nil
	}, Run: func(context.Context, Session) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		left, right := sshtest.Pipe()
		done := make(chan error, 1)
		go func() { done <- s.Handle(t.Context(), right) }()
		native, channels, requests, err := ssh.NewClientConn(left, "local:22", &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())})
		if err != nil {
			t.Fatal(err)
		}
		client := ssh.NewClient(native, channels, requests)
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close(); _ = client.Close(); <-done })
	}
}
