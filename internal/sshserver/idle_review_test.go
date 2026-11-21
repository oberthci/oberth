package sshserver

import (
	"context"
	"k8s.io/utils/clock"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
)

func TestReviewAuthenticatedSilentSessionIdle(t *testing.T) { checkSilentSSH(t, true) }
func TestAuthenticatedConnectionBeforeFirstChannelHasIdleLimit(t *testing.T) {
	checkSilentSSH(t, false)
}
func checkSilentSSH(t *testing.T, openChannel bool) {
	t.Helper()
	host, clientKey := signer(t), signer(t)
	server, err := New(Config{Signer: host, Now: (clock.RealClock{}).Now, Idle: 200 * time.Millisecond, Total: 4 * time.Second,
		Lookup: func(context.Context, ssh.PublicKey) (Identity, error) {
			return Identity{Actor: "agent", Fingerprint: "key", CanAccess: func(string) bool { return true }}, nil
		}, Run: func(context.Context, Session) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := sshtest.Pipe()
	defer func() { _ = clientConn.Close() }()
	done := make(chan error, 1)
	start := (clock.RealClock{}).Now()
	go func() { done <- server.Handle(context.Background(), serverConn) }()
	native, ch, req, err := ssh.NewClientConn(clientConn, "local:22", &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(native, ch, req)
	if openChannel {
		if _, err = client.NewSession(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
		t.Logf("silent authenticated session closed after %v (idle 200ms, total 4s)", (clock.RealClock{}).Now().Sub(start).Round(10*time.Millisecond))
	case <-time.After(time.Second):
		t.Fatal("silent session exceeded its idle deadline")
	}
	_ = client.Close()
}
