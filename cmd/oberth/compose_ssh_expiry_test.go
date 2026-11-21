//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestComposedInvitedUplinkExpiresOverSSHAndRenewalExtendsIt(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.deps.clock.Now = f.clock.Now
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.enrollJoined(t)
	key := readSSHSigner(t, f.identity)
	f.assertSSHAuthentication(t, []ssh.Signer{key}, true)
	first := f.joinedCertificate(t).Leaf
	uplink, err := f.owners.c.store.Key(t.Context(), ssh.FingerprintSHA256(key.PublicKey()))
	if err != nil || !uplink.ExpiresAt.Equal(first.NotAfter) {
		t.Fatal("joined uplink did not persist its certificate lifetime", err)
	}
	f.clock.Advance(time.Hour)
	renewInstalledClient(t, f, filepath.Dir(f.identity))
	renewed := f.joinedCertificate(t).Leaf
	if !renewed.NotAfter.After(first.NotAfter) {
		t.Fatal("renewal did not extend expiry")
	}
	f.clock.Advance(first.NotAfter.Sub(f.clock.Now()))
	f.assertSSHAuthentication(t, []ssh.Signer{key}, true)
	f.clock.Advance(renewed.NotAfter.Sub(f.clock.Now()))
	f.assertSSHAuthentication(t, []ssh.Signer{key}, false)
}
