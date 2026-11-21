package sshserver_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sshserver"
	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/sys/unix"
	"k8s.io/utils/clock"
)

func TestBlockingGitInputReturnsAfterRemoteRefusal(t *testing.T) {
	for _, verb := range []string{"git-upload-pack", "git-receive-pack"} {
		t.Run(verb, func(t *testing.T) { refusedSession(t, verb) })
	}
}

func refusedSession(t *testing.T, verb string) {
	t.Helper()
	cfg, server := refusalEndpoint(t)
	left, right := sshtest.Pipe()
	cfg.Dial = func(context.Context, string) (net.Conn, error) { return left, nil }
	cfg.Command = verb + " '/origin/team/widget.git'"
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Handle(t.Context(), right) }()
	var descriptors [2]int
	if err := unix.Pipe(descriptors[:]); err != nil {
		t.Fatal(err)
	}
	input := os.NewFile(uintptr(descriptors[0]), "native-git-input")
	writer := os.NewFile(uintptr(descriptors[1]), "native-git-writer")
	defer func() { _ = writer.Close(); _ = input.Close(); _ = left.Close(); <-serverDone }()
	var diagnostic bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- gitssh.Run(t.Context(), cfg, gitssh.IO{Input: input, Output: io.Discard, Diagnostics: &diagnostic})
	}()
	select {
	case err := <-done:
		var status *ssh.ExitError
		if !errors.As(err, &status) || status.ExitStatus() != 1 || diagnostic.String() != "unavailable: Forge unavailable. - Retry later.\n" {
			t.Fatalf("refusal: %v; %q", err, diagnostic.String())
		}
	case <-time.After(5 * time.Second):
		_ = writer.Close()
		<-done
		t.Fatal("client waited for Git input after the server ended the session")
	}
}

func refusalEndpoint(t *testing.T) (gitssh.Config, *sshserver.Server) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	identity, pins := filepath.Join(dir, "identity"), filepath.Join(dir, "pins")
	if err = os.WriteFile(identity, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pins, []byte(knownhosts.Line([]string{"local"}, signer.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server, err := sshserver.New(sshserver.Config{Signer: signer, Now: clock.RealClock{}.Now,
		Lookup: func(context.Context, ssh.PublicKey) (sshserver.Identity, error) {
			return sshserver.Identity{Actor: "caller", CanAccess: func(string) bool { return true }}, nil
		}, Run: func(context.Context, sshserver.Session) error {
			return publicerr.New(publicerr.Unavailable, "Forge unavailable.", "Retry later.", nil)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return gitssh.Config{Host: "local", Port: "22", User: "git", Identity: identity, KnownHosts: pins, Scratch: dir, Now: clock.RealClock{}.Now}, server
}
