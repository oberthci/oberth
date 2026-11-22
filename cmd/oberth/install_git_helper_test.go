package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/installer"
)

func TestUpgradeRollbackAndInstallRerunKeepTheProfileGitHelperWorking(t *testing.T) {
	f := signedMaintenanceFixture(t)
	app, _, diagnostic := f.rollbackApplication(t)
	clients := helperClients(t, "bound", "upgrade", "rollback", "install")
	bindProfileHelper(t, app, clients["bound"])
	f.wireGitHelper(&app, clients["upgrade"])
	before := profileKey(t, app)
	upgradeSignedFixture(t, &f)
	if after := profileKey(t, app); after != before || !fileExists(before) {
		t.Fatal("upgrade renamed the reused administrator key", before, after)
	}
	assertHelperOnProfileKey(t, app, clients["upgrade"])
	f.wireGitHelper(&app, clients["rollback"])
	if code := app.run(t.Context(), []string{"rollback", "--release-dir", f.previous, "--yes"}); code != 0 {
		t.Fatal("rollback failed", code, diagnostic)
	}
	assertHelperOnProfileKey(t, app, clients["rollback"])
	f.wireGitHelper(&app, clients["install"])
	before = profileKey(t, app)
	rerunSignedInstall(t, f, "v0.1.0", f.previous)
	if after := profileKey(t, app); after != before || !fileExists(before) {
		t.Fatal("install rerun renamed the reused administrator key", before, after)
	}
	assertHelperOnProfileKey(t, app, clients["install"])
	if strings.Contains(diagnostic.String(), "not repaired") {
		t.Fatal("repair reported failure", diagnostic)
	}
}

func TestUnrepairableGitHelperKeepsItsKeyAndNamesTheRepair(t *testing.T) {
	f := signedMaintenanceFixture(t)
	app, _, diagnostic := f.rollbackApplication(t)
	clients := helperClients(t, "bound")
	bindProfileHelper(t, app, clients["bound"])
	app.executable = func() (string, error) { return "", errors.New("client path unavailable") }
	session := f.session
	session.app = app
	f.session.engine.GitHelper = session.repointGit
	before := profileKey(t, app)
	certificate := installedProfile(t, app).AdminCert
	upgradeSignedFixture(t, &f)
	if profileKey(t, app) != before || installedProfile(t, app).AdminCert == certificate {
		t.Fatal("upgrade renamed the reused key or kept the earlier certificate name")
	}
	helper := profileHelper(t, app)
	if helper.Identity != before || !fileExists(before) {
		t.Fatal("unrepaired helper lost the key it names", helper.Identity)
	}
	dir := filepath.Dir(before)
	if diagnostic.String() != "Git helper not repaired.\nIn one checkout of this profile, run oberth repo add <forge URL>.\nEarlier credential files are kept in "+dir+".\n" {
		t.Fatal("failure does not name the repair and the kept files", diagnostic)
	}
	kept, err := installer.EarlierCredentials(installedProfile(t, app))
	if err != nil || !slices.Contains(kept, certificate) {
		t.Fatal("earlier certificate not kept", kept, err)
	}
	f.wireGitHelper(&app, clients["bound"])
	rerunSignedInstall(t, f, "v0.2.0", f.target)
	assertHelperOnProfileKey(t, app, clients["bound"])
	if kept, err = installer.EarlierCredentials(installedProfile(t, app)); err != nil || len(kept) != 0 {
		t.Fatal("successful repair left earlier credential files", kept, err)
	}
}

func TestUnboundProfileGetsNoGitHelper(t *testing.T) {
	f := signedMaintenanceFixture(t)
	app, _, diagnostic := f.rollbackApplication(t)
	f.wireGitHelper(&app, helperClients(t, "upgrade")["upgrade"])
	upgradeSignedFixture(t, &f)
	if fileExists(filepath.Join(app.base, "installed", "ssh.config")) || diagnostic.Len() != 0 {
		t.Fatal("an unbound profile gained a Git helper", diagnostic)
	}
}

func TestInstallationEngineCarriesTheGitHelperRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	data := `{"apiVersion":"v1","kind":"Config","clusters":[{"name":"test","cluster":{"server":"https://127.0.0.1:1"}}],"contexts":[{"name":"test","context":{"cluster":"test"}}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	app := application{base: t.TempDir(), env: func(name string) string {
		if name == "KUBECONFIG" {
			return path
		}
		return ""
	}}
	s := installationSession{app: app, choices: installer.Choices{Context: "test"}, name: "wired"}
	if err := s.openEngine(); err != nil {
		t.Fatal(err)
	}
	defer s.engine.Credentials.Close()
	if s.engine.GitHelper == nil {
		t.Fatal("installation engine leaves the profile's Git helper unrepaired")
	}
}

func (f signedMaintenance) wireGitHelper(app *application, executable string) {
	app.executable = func() (string, error) { return executable, nil }
	session := f.session
	session.app = *app
	f.session.engine.GitHelper = session.repointGit
}

func helperClients(t *testing.T, names ...string) map[string]string {
	t.Helper()
	clients := map[string]string{}
	for _, name := range names {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		clients[name] = filepath.Join(dir, "oberth")
		if err := os.WriteFile(clients[name], []byte("#!/bin/sh\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return clients
}

// bindProfileHelper writes the helper exactly as repo add binds a checkout.
func bindProfileHelper(t *testing.T, app application, executable string) {
	t.Helper()
	app.executable = func() (string, error) { return executable, nil }
	profile := installedProfile(t, app)
	session := &flowSession{app: app, ctx: t.Context(), resolved: client.Options{URL: profile.ServerURL, ClientKey: profile.AdminKey}}
	if _, _, err := session.checkoutConnection(profile.KnownHosts); err != nil {
		t.Fatal(err)
	}
	assertHelperOnProfileKey(t, app, executable)
}

func rerunSignedInstall(t *testing.T, f signedMaintenance, version, directory string) {
	t.Helper()
	release, err := installer.LoadRelease(t.Context(), os.DirFS(directory), version)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.session.engine.Prepare(t.Context(), release, f.session.choices)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if _, err = f.session.engine.Apply(t.Context(), plan, plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
}

func assertHelperOnProfileKey(t *testing.T, app application, executable string) {
	t.Helper()
	profile := installedProfile(t, app)
	helper := profileHelper(t, app)
	if helper.Identity != profile.AdminKey || !fileExists(helper.Identity) {
		t.Fatal("Git helper does not name the profile's key", helper.Identity, profile.AdminKey)
	}
	if !fileExists(helper.KnownHosts) {
		t.Fatal("Git helper names a missing host-key file", helper.KnownHosts)
	}
	target, err := os.Readlink(filepath.Join(app.base, "installed", "git-ssh"))
	if err != nil || target != executable {
		t.Fatal("Git helper does not run the client that ran the command", target, err)
	}
}

func profileHelper(t *testing.T, app application) gitssh.Config {
	t.Helper()
	endpoint, err := url.Parse(installedProfile(t, app).SSHURL)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := gitssh.Configure(filepath.Join(app.base, "installed", "ssh.config"), gitssh.Config{Host: endpoint.Hostname()})
	if err != nil {
		t.Fatal(err)
	}
	return helper
}

func installedProfile(t *testing.T, app application) installer.Profile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(app.base, "installed", "installation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var profile installer.Profile
	if err = json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	return profile
}

func profileKey(t *testing.T, app application) string {
	t.Helper()
	return installedProfile(t, app).AdminKey
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
