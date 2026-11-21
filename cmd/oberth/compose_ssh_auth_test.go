//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/sshserver"
	"github.com/oberthci/oberth/internal/sshserver/sshtest"
	"github.com/oberthci/oberth/internal/stateport"
	"golang.org/x/crypto/ssh"
)

func TestComposedUnknownAndRevokedKeysReceiveSSHAuthRefusal(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	path, key := composeSSHKey(t, t.TempDir(), "unknown")
	bad := readSSHSigner(t, path)
	good := readSSHSigner(t, f.identity)
	for _, revoked := range []bool{false, true} {
		if revoked {
			action := stateport.Action{Actor: "operator"}
			fingerprint := ssh.FingerprintSHA256(key)
			if err := f.owners.c.store.RegisterKey(t.Context(), action, stateport.Key{Identity: "refused", Fingerprint: fingerprint, Kind: "agent", ParkOnPush: true}); err != nil {
				t.Fatal(err)
			}
			if err := f.owners.c.store.RevokeKey(t.Context(), action, fingerprint); err != nil {
				t.Fatal(err)
			}
		}
		for _, retry := range []bool{false, true} {
			keys := []ssh.Signer{bad}
			if retry {
				keys = append(keys, good)
			}
			f.assertSSHAuthentication(t, keys, retry)
		}
	}
}

func readSSHSigner(t *testing.T, path string) ssh.Signer {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func (f *composedFixture) assertSSHAuthentication(t *testing.T, keys []ssh.Signer, allowed bool) {
	t.Helper()
	host := f.owners.c.hostKey
	server, err := sshserver.New(sshserver.Config{Signer: host, Lookup: f.owners.sshIdentity, Now: f.deps.clock.Now, Run: func(context.Context, sshserver.Session) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	client, peer := sshtest.Pipe()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Handle(t.Context(), peer) }()
	native, _, _, err := ssh.NewClientConn(client, "local:22", &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(keys...)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())})
	if native != nil {
		_ = native.Close()
	}
	_ = client.Close()
	<-stopped
	if allowed && err != nil {
		t.Fatal("valid second key was not tried", err)
	}
	if !allowed && (err == nil || !strings.Contains(err.Error(), "unable to authenticate")) {
		t.Fatal("lookup denial did not produce SSH authentication refusal", err)
	}
}
