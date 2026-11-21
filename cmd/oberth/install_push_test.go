//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestInstalledAdministratorPushUsesRecordedKey(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.config.Public.URL = "https://127.0.0.1"
	port := reserveInstalledSSH(t, f)
	profile := installComposedProfile(t.Context(), t, f, port)
	f.prepareCommit(t)
	close(f.allowList)
	cancel, done := f.run(t)
	defer cancel()
	f.waitStarted(t, done)
	app := application{directory: f.git.Work, executable: func() (string, error) { return f.binary, nil }}
	s := flowSession{app: app, ctx: t.Context(), resolved: client.Options{ClientKey: profile.AdminKey, URL: profile.ServerURL}}
	if err := s.bindCheckout("origin/team/widget", profile.KnownHosts); err != nil {
		t.Fatal("installed profile could not bind checkout", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(profile.AdminKey), client.UplinkPushKeyFile)); !os.IsNotExist(err) {
		t.Fatal("installation unexpectedly supplied a second push key")
	}
	f.git.Git(t, f.git.Work, "push", "-o", "oberth.run=1", "oberth", "HEAD:refs/heads/installed-admin")
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if run.Origin.Actor != "install-admin" {
		t.Fatal("push did not use the installed administrator", run.Origin.Actor)
	}
}

func reserveInstalledSSH(t *testing.T, f *composedFixture) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	listen := f.deps.listen
	f.deps.listen = func(ctx context.Context, network, address string) (net.Listener, error) {
		if address == f.config.Listen.SSH {
			f.ssh = listener
			return listener, nil
		}
		return listen(ctx, network, address)
	}
	return number
}
