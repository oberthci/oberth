package main

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	"golang.org/x/crypto/ssh"
)

// The server clones a plain SSH forge's repository as the forge's own user,
// from the namespace the upstream was registered with.
func TestServerReachesAPlainSSHForgeAsItsOwnUser(t *testing.T) {
	forge := composeForge(t, gittest.New(t))
	dir := t.TempDir()
	key, _ := composeSSHKey(t, t.TempDir(), "forge")
	c := &composition{config: serverConfig{DatabaseFile: filepath.Join(dir, "state.db"), Files: serverFiles{ForgeKey: key}}}
	owners := &serverOwners{c: c, settings: serverRuntime{DefaultBranch: "refs/heads/main"}}
	a := serverAdministration{owners}
	base := "ssh://oberth@" + forge.address + "/home/oberth/repos"
	prepared, err := a.PrepareUpstream(t.Context(), apicontract.UpstreamAddArgs{Name: "plain", BaseURL: base, CheckRepo: "repos/oberth-smoke", ExpectedHostFingerprint: ssh.FingerprintSHA256(forge.key)})
	if err != nil || prepared.Persist(t.Context()) != nil {
		t.Fatal(err)
	}
	if prepared.Binding.Org != "repos" || prepared.Binding.BaseURL != base {
		t.Fatal("upstream lost the forge's user or namespace", prepared.Binding)
	}
	db, err := store.Create(t.Context(), c.config.DatabaseFile, store.DefaultOptions(fake.NewClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, prepared.Binding); err != nil {
		t.Fatal(err)
	}
	c.store = db
	remote, err := owners.upstream(t.Context(), "plain/repos/oberth-smoke")
	if err != nil || remote.URL != "ssh://oberth@127.0.0.1/home/oberth/repos/oberth-smoke.git" {
		t.Fatal("clone address differs", remote.URL, err)
	}
	_, port, _ := net.SplitHostPort(forge.address)
	shim, err := gitssh.Configure(remote.SSHConfig, gitssh.Config{Host: "127.0.0.1"})
	if err != nil || shim.User != "oberth" || shim.Port != port {
		t.Fatal("git-ssh shim reaches another account", shim.User, shim.Port, err)
	}
}
