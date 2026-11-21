package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/sshserver"
	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"golang.org/x/crypto/ssh"
)

// Covers: REQ-C6-001
func TestExplicitGitIdentitiesContract(t *testing.T) {
	t.Run("configured host identity", configuredHostIdentities)
	t.Run("fresh post-proof scope", changedScopeAfterProof)
}
func configuredHostIdentities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_config")
	var text strings.Builder
	for _, host := range []string{"first", "second"} {
		text.WriteString("Host " + host + "\n IdentityFile /keys/" + host + "\n UserKnownHostsFile /pins/" + host + "\n IdentitiesOnly yes\n BatchMode yes\n StrictHostKeyChecking yes\n GlobalKnownHostsFile /dev/null\n")
	}
	if err := os.WriteFile(path, []byte(text.String()), 0600); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"first", "second"} {
		c, err := gitssh.Configure(path, gitssh.Config{Host: host})
		if err != nil || c.Identity != "/keys/"+host || c.KnownHosts != "/pins/"+host {
			t.Fatalf("%+v %v", c, err)
		}
	}
}
func changedScopeAfterProof(t *testing.T) {
	dir := t.TempDir()
	host, key := privateKey(t, dir, "host"), privateKey(t, dir, "key")
	left, right := sshtest.Pipe()
	defer func() { _ = left.Close(); _ = right.Close() }()
	calls, executions := 0, 0
	server, err := sshserver.New(sshserver.Config{Signer: host, Now: func() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }, Lookup: func(context.Context, ssh.PublicKey) (sshserver.Identity, error) {
		calls++
		scope := "origin/team/widget"
		if calls > 1 {
			scope = "origin/team/other"
		}
		return sshserver.Identity{Actor: "agent", Fingerprint: "key", CanAccess: func(repo string) bool { return repo == scope }}, nil
	}, Run: func(context.Context, sshserver.Session) error { executions++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Handle(t.Context(), right) }()
	native, channels, requests, err := ssh.NewClientConn(left, "local:22", &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	client := ssh.NewClient(native, channels, requests)
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Run("git-upload-pack 'origin/team/widget.git'"); err == nil {
		t.Fatal("stale probe scope authorized a Git command")
	}
	_ = session.Close()
	_ = client.Close()
	<-done
	if calls != 2 || executions != 0 {
		t.Fatalf("lookups=%d executions=%d", calls, executions)
	}
}
