//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"context"
	"k8s.io/client-go/rest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/client"
	"golang.org/x/crypto/ssh"
)

func TestRepoAddConfirmsOfferedFingerprintAndExplicitRepin(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	freshRepoAddCache(f)
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	close(f.allowList)
	cancel, done := f.run(t)
	defer cancel()
	f.waitStarted(t, done)
	command := newRepoAddCommand(t, f)
	if code := command.run(t); code != 1 || !strings.Contains(command.diagnostic.String(), ssh.FingerprintSHA256(f.forge.key)) {
		t.Fatal("first attempt did not show offered fingerprint", code, command.diagnostic.String())
	}
	_, retry, found := strings.Cut(command.diagnostic.String(), "Retry:\n")
	retry = strings.SplitN(retry, "\n", 2)[0]
	if !found || !strings.HasPrefix(retry, "oberth repo add '") || !strings.Contains(retry, "--expected-host-fingerprint VERIFIED_FINGERPRINT") {
		t.Error("retry command was not copyable", command.diagnostic.String())
	}
	_, wrong := composeSSHKey(t, t.TempDir(), "wrong")
	if code := command.run(t, "--expected-host-fingerprint", ssh.FingerprintSHA256(wrong)); code != 1 {
		t.Error("wrong fingerprint was not refused by the service", code, command.diagnostic.String())
	}
	if code := command.run(t, "--expected-host-fingerprint", ssh.FingerprintSHA256(f.forge.key)); code != 0 {
		t.Fatal("verified fingerprint did not connect", code, command.diagnostic.String())
	}
	if repo, err := runGit(t.Context(), command.app.directory, "config", "--get", "oberth.repository"); err != nil || repo != "127-0-0-1/team/widget" {
		t.Fatal("checkout was not bound", repo, err)
	}
	assertCutoverUpstreamOutput(t, command, "127-0-0-1")
	command.checkRotation(t, f)
}

func newRepoAddCommand(t *testing.T, f *composedFixture) repoAddCommand {
	t.Helper()
	app, out, diagnostic := fixture(t, fake.Bindings())
	_, profile := joinAdministratorProfile(t, &app, "install")
	if err := os.Remove(filepath.Join(filepath.Dir(profile.AdminKey), "token")); err != nil {
		t.Fatal(err)
	}
	app.config.Now = f.deps.clock.Now
	app.config.Transport = f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport
	app.directory = f.git.Work
	return repoAddCommand{app, out, diagnostic, "ssh://" + f.forge.address + "/team/widget.git", profile.KnownHosts}
}

func (c *repoAddCommand) checkRotation(t *testing.T, f *composedFixture) {
	t.Helper()
	keyPath, key := composeSSHKey(t, t.TempDir(), "changed-host")
	raw, err := client.ReadPrivate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	f.forge.config.Store(config)
	c.app.directory = t.TempDir()
	if _, err = runGit(t.Context(), c.app.directory, "init"); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--expected-host-fingerprint", ssh.FingerprintSHA256(key)}
	if code := c.run(t, flags...); code != 1 || !strings.Contains(c.diagnostic.String(), "--repin") {
		t.Fatal("changed key accepted without repin", code, c.diagnostic.String())
	}
	if code := c.run(t, append(flags, "--repin")...); code != 0 {
		t.Fatal("confirmed changed key did not reconnect", code, c.diagnostic.String())
	}
}

func freshRepoAddCache(f *composedFixture) {
	platform := serverPlatform{executable: f.binary, cluster: func(context.Context) (clusterClients, error) {
		return clusterClients{Kube: f.kube, Argo: f.argo, REST: &rest.Config{Host: "https://cluster.oberth.ci"}}, nil
	}}
	build := platform.constructors().CacheNew
	f.deps.constructors.CacheNew = func(ctx context.Context, c *composition) (Part, error) {
		part, err := build(ctx, c)
		f.owners = c.owners
		return part, err
	}
}
