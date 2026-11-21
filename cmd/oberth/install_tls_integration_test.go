//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
)

func TestInstalledCAJoinRenewalAndPin(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	profile := installComposedProfile(t.Context(), t, f, 30022)
	var err error
	f.adminCertificate, err = tls.LoadX509KeyPair(profile.AdminCert, profile.AdminKey)
	if err != nil {
		t.Fatal(err)
	}
	close(f.allowList)
	cancel, done := f.run(t)
	defer cancel()
	f.waitStarted(t, done)
	invitation := f.invitation(t, "installed-join")
	dir := filepath.Join(t.TempDir(), "joined")
	if err = os.Chmod(filepath.Dir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	config := client.Config{Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS).Transport}
	wrong := f.invitation(t, "wrong-pin-join")
	wrong.CAPin = strings.Repeat("0", 64)
	if _, err = client.Join(t.Context(), config, wrong, filepath.Join(filepath.Dir(dir), "refused")); err == nil {
		t.Fatal("incorrect CA pin accepted")
	}
	if _, err = client.Join(t.Context(), config, invitation, dir); err != nil {
		t.Fatal("installed CA join", err)
	}
	renewInstalledClient(t, f, dir)
}
