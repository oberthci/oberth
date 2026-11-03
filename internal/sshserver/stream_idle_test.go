package sshserver

import (
	"context"
	"github.com/oberthci/oberth/internal/gitcache"
	"io"
	"testing"
	"time"
)

type idleStreamGit struct {
	fakeGit
	started chan struct{}
}

func (g *idleStreamGit) Serve(ctx context.Context, _ string, _ gitcache.Service, _ string, input io.Reader, _, _ io.Writer) error {
	if transport, ok := input.(interface {
		BeginTransport()
		EndTransport()
	}); ok {
		transport.BeginTransport()
		defer transport.EndTransport()
	}
	close(g.started)
	_, err := io.Copy(io.Discard, input)
	return err
}
func TestGitStreamIdleDeadlineClosesStalledSession(t *testing.T) {
	signer := newSigner(t)
	git := &idleStreamGit{started: make(chan struct{})}
	server, err := New(Config{HostSigner: newSigner(t), Resolver: IdentityResolverFunc(func(string) (string, bool, error) { return "actor", true, nil }), Git: git, OnUpdate: noOpUpdateHandler(), StreamIdleTimeout: 100 * time.Millisecond, SessionTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client, stop := startServer(t, server, signer)
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Start("git-upload-pack 'example'"); err != nil {
		t.Fatal(err)
	}
	<-git.started
	// Actual Git stream traffic renews the idle deadline beyond its first expiry.
	for range 4 {
		if _, err = stdin.Write([]byte("progress")); err != nil {
			t.Fatalf("active stream expired: %v", err)
		}
		time.Sleep(40 * time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle stream survived deadline")
	}
}
