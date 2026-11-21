package sshserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
)

func signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIncomingScopeIsRecheckedAfterKeyProof(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{true: "revoked", false: "accepted"}[revoked], func(t *testing.T) { checkProof(t, revoked) })
	}
}
func checkProof(t *testing.T, revoked bool) {

	calls := 0
	host, clientKey := signer(t), signer(t)
	server, err := New(Config{Signer: host, Now: func() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }, Lookup: func(context.Context, ssh.PublicKey) (Identity, error) {
		calls++
		if revoked && calls == 2 {
			return Identity{}, failure(reasonIdentity)
		}
		return Identity{Actor: "agent", Fingerprint: "key", CanAccess: func(repo string) bool { return repo == "origin/team/widget" }}, nil
	}, Run: func(context.Context, Session) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := sshtest.Pipe()
	defer func() { _ = clientConn.Close(); _ = serverConn.Close() }()
	done := make(chan error, 1)
	go func() { done <- server.Handle(t.Context(), serverConn) }()
	native, ch, req, err := ssh.NewClientConn(clientConn, "local:22", &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())})
	if revoked {
		if err == nil {
			_ = native.Close()
			t.Fatal("revoked proof accepted")
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		client := ssh.NewClient(native, ch, req)
		_ = client.Close()
	}
	<-done
	if calls != 2 {
		t.Fatalf("lookup calls %d", calls)
	}
}

// Covers: REQ-C6-008
func TestBoundedGitTransportContract(t *testing.T) {
	checkTransportDeadline(t)
	fired := 0
	now := time.Unix(0, 0)
	c := newIdle(nil, time.Hour, func() { fired++ }, func() time.Time { return now })
	c.fire()
	if fired != 0 {
		t.Fatal("preparation counted as transport")
	}
	c.begin()
	c.progress(1)
	now = now.Add(time.Hour)
	c.fire()
	if fired != 1 {
		t.Fatal("idle transport did not expire")
	}
	c.end()
	c.fire()
	if fired != 1 {
		t.Fatal("finalization counted as transport")
	}
	c.close()
}

func TestCommandRefusesArgumentAndPathInjection(t *testing.T) {
	if _, err := ParseCommand("git-upload-pack '/origin/team/widget.git'"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCommand("git-upload-pack 'origin/team/widget.git'"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"sh -c anything", "git-upload-pack '--help'", "git-upload-pack 'origin/team/widget'; touch outside", "git-upload-pack '../outside'", "git-upload-pack 'origin/team/widget' extra"} {
		if _, err := ParseCommand(s); err == nil {
			t.Fatal(s)
		}
	}
}

func TestIdentityOutageSendsTemporaryBannerBeforeClosing(t *testing.T) {
	server, err := New(Config{Signer: signer(t), Now: func() time.Time { return time.Unix(100, 0) }, Lookup: func(context.Context, ssh.PublicKey) (Identity, error) { return Identity{}, io.ErrUnexpectedEOF }, Run: func(context.Context, Session) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	conn := &closeRecorder{}
	preauth := &bannerRecorder{}
	identity := Identity{}
	config := server.handshakeConfig(t.Context(), conn, &identity)
	config.PreAuthConnCallback(preauth)
	if _, err = config.PublicKeyCallback(nil, signer(t).PublicKey()); err == nil {
		t.Fatal("lookup outage accepted")
	}
	if !conn.closed || !strings.Contains(preauth.banner, reasonTemporary) || strings.Contains(preauth.banner, io.ErrUnexpectedEOF.Error()) {
		t.Fatalf("closed=%v, banner=%q", conn.closed, preauth.banner)
	}
}

type closeRecorder struct {
	net.Conn
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

type bannerRecorder struct {
	ssh.ServerPreAuthConn
	banner string
}

func (b *bannerRecorder) SendAuthBanner(s string) error { b.banner = s; return nil }
