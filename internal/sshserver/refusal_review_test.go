package sshserver

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
	"k8s.io/utils/clock"
)

func TestNonSessionChannelIsRefused(t *testing.T) {
	host, clientKey := signer(t), signer(t)
	server, err := New(Config{Signer: host, Now: (clock.RealClock{}).Now, Lookup: func(context.Context, ssh.PublicKey) (Identity, error) {
		return Identity{Actor: "actor", CanAccess: func(string) bool { return true }}, nil
	}, Run: func(context.Context, Session) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	left, right := sshtest.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.Handle(t.Context(), right) }()
	conn, channels, requests, err := ssh.NewClientConn(left, "local:22", &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(conn, channels, requests)
	defer func() { _ = client.Close(); <-done }()
	channel, _, err := client.OpenChannel("direct-tcpip", nil)
	if err == nil {
		_ = channel.Close()
		t.Fatal("unsupported channel accepted")
	}
}
func TestEmptyActorAndZeroByteProgressCannotHoldSession(t *testing.T) {
	server := &Server{config: Config{Lookup: func(context.Context, ssh.PublicKey) (Identity, error) {
		return Identity{CanAccess: func(string) bool { return true }}, nil
	}}}
	if _, err := server.lookup(t.Context(), signer(t).PublicKey()); err == nil {
		t.Fatal("empty actor accepted")
	}
	now := time.Unix(0, 0)
	channel := newIdle(nil, time.Hour, func() { t.Error("idle timer fired during fixture") }, func() time.Time { return now })
	channel.begin()
	defer channel.close()
	deadline := channel.deadline
	now = now.Add(time.Minute)
	channel.progress(0)
	if !channel.deadline.Equal(deadline) {
		t.Fatal("zero bytes extended inactivity deadline")
	}
}
