package sshserver

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
)

func TestTwentyRegisteredClientsFromOneAddressAuthenticate(t *testing.T) {
	s, config := populationServer(t)
	var clients []net.Conn
	for range 20 {
		left, _, _ := populationConnection(t, s, 1)
		clients = append(clients, left)
	}
	for _, conn := range clients {
		authenticatePopulation(t, conn, config)
	}
}

func TestSilentAddressReleasesBudgetAtHandshakeDeadline(t *testing.T) {
	s, config := populationServer(t)
	var silent []*deadlineConn
	var finished []<-chan error
	for range 32 {
		_, conn, done := populationConnection(t, s, 1)
		silent = append(silent, conn)
		finished = append(finished, done)
	}
	other, _, _ := populationConnection(t, s, 2)
	authenticatePopulation(t, other, config)
	for _, conn := range silent {
		if !conn.last().Equal(s.config.Now().Add(5 * time.Second)) {
			t.Fatal("unauthenticated connection has no short deadline")
		}
		// Expire the installed deadline without waiting on wall time.
		if err := conn.Conn.SetDeadline(time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	for _, done := range finished {
		if err := <-done; err == nil {
			t.Fatal("silent connection authenticated")
		}
	}
	same, _, _ := populationConnection(t, s, 1)
	authenticatePopulation(t, same, config)
}

func populationServer(t *testing.T) (*Server, *ssh.ClientConfig) {
	t.Helper()
	host, key := signer(t), signer(t)
	s, err := New(Config{Signer: host, Now: func() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) },
		Lookup: func(_ context.Context, presented ssh.PublicKey) (Identity, error) {
			if ssh.FingerprintSHA256(presented) != ssh.FingerprintSHA256(key.PublicKey()) {
				t.Error("unregistered key authenticated")
			}
			return Identity{Actor: "agent", CanAccess: func(string) bool { return true }}, nil
		}, Run: func(context.Context, Session) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return s, &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())}
}

func populationConnection(t *testing.T, s *Server, address byte) (net.Conn, *deadlineConn, <-chan error) {
	t.Helper()
	left, right := sshtest.Pipe()
	conn := &deadlineConn{Conn: addressedConn{Conn: right, address: &net.TCPAddr{IP: net.IPv4(192, 0, 2, address), Port: 1234}}}
	ready := make(chan struct{})
	observed := &handshakeReady{deadlineConn: conn, ready: ready}
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { done <- s.Handle(t.Context(), observed); close(exited) }()
	t.Cleanup(func() { _ = left.Close(); <-exited })
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("connection refused below address capacity: %v", err)
	}
	return left, conn, done
}

type handshakeReady struct {
	*deadlineConn
	ready chan struct{}
}

func (c *handshakeReady) SetDeadline(at time.Time) error {
	err := c.deadlineConn.SetDeadline(at)
	if c.ready != nil {
		close(c.ready)
		c.ready = nil
	}
	return err
}

func authenticatePopulation(t *testing.T, conn net.Conn, config *ssh.ClientConfig) {
	t.Helper()
	native, channels, requests, err := ssh.NewClientConn(conn, "local:22", config)
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(native, channels, requests)
	t.Cleanup(func() { _ = client.Close() })
}
