//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

type joinedAgent struct {
	app             application
	out, diagnostic *bytes.Buffer
	clone           string
}

// The join file carries the installation's SSH host keys: in an empty home a
// joined identity binds a fresh clone with plain repo add and pushes, and a
// key the installation does not hold is never trusted.
func TestJoinedFreshHomeAddsItsRepositoryWithTheInvitedHostKeys(t *testing.T) {
	t.Parallel()
	w := newReadmeWalk(t)
	w.runCommands(t, readmeFirstDayCommands(t)[:3])
	trusted, err := client.ReadPublic(w.known)
	if err != nil {
		t.Fatal(err)
	}
	forge := "ssh://" + w.f.forge.address + "/team/widget.git"
	endpoint := strings.Fields(string(trusted))[0]

	other := w.joinFresh(t, "agent-other-host", func(text string) string { return strings.Replace(text, endpoint, "[127.0.0.2]:22", 1) })
	if code := other.app.run(t.Context(), []string{"repo", "add", forge}); code != 1 || !strings.Contains(other.diagnostic.String(), "SSH trust file has no usable endpoint key.") {
		t.Fatal("repo add trusted keys for another host", code, other.diagnostic.String())
	}

	forged := w.joinFresh(t, "agent-other-key", func(string) string {
		return endpoint + " " + strings.Fields(fixtureHostKeys)[1] + " " + strings.Fields(fixtureHostKeys)[2] + "\n"
	})
	if code := forged.app.run(t.Context(), []string{"repo", "add", forge}); code != 0 {
		t.Fatal("repo add did not bind with the invited key", code, forged.diagnostic.String())
	}
	if pushed, err := gitIn(t, w.f, forged.clone, "push", "oberth", "HEAD:refs/heads/forged", "-o", "oberth.run=1"); err == nil || !strings.Contains(pushed, "host key") {
		t.Fatal("push reached a server whose key the invitation does not hold", pushed, err)
	}

	agent := w.joinFresh(t, "agent-fresh", func(text string) string { return text })
	if code := agent.app.run(t.Context(), []string{"repo", "add", forge}); code != 0 || !strings.Contains(agent.out.String(), "bound:") {
		t.Fatal("joined identity could not add its repository", code, agent.diagnostic.String())
	}
	if pushed, err := gitIn(t, w.f, agent.clone, "push", "oberth", "HEAD:refs/heads/fresh", "-o", "oberth.run=1"); err != nil {
		t.Fatal("joined push failed", pushed, err)
	}
	run := w.f.waitRun(t, func(r stateport.Run) bool { return r.Ref == "refs/heads/fresh" })
	if run.Origin.Actor != "agent-fresh" {
		t.Fatal("push did not use the joined identity", run.Origin.Actor)
	}
}

// joinFresh issues an invitation, lets edit change its host keys, and joins it
// in an empty home from a fresh clone that has no binding.
func (w *readmeWalk) joinFresh(t *testing.T, identity string, edit func(string) string) joinedAgent {
	t.Helper()
	f := w.f
	w.out.Reset()
	w.diagnostic.Reset()
	if code := w.app.run(t.Context(), []string{"invite", "127-0-0-1", "--identity", identity}); code != 0 {
		t.Fatal("invite failed", code, w.diagnostic.String())
	}
	w.readInvitation(t)
	editInvitation(t, w.invitation, edit)
	home, base := privateDirectory(t), privateDirectory(t)
	clone := filepath.Join(t.TempDir(), "widget")
	f.git.Git(t, filepath.Dir(clone), "clone", "-q", f.git.Bare, clone)
	agent := joinedAgent{out: &bytes.Buffer{}, diagnostic: &bytes.Buffer{}, clone: clone}
	agent.app = application{base: base, directory: clone, in: strings.NewReader(""), out: agent.out, diagnostic: agent.diagnostic, env: func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	}, executable: func() (string, error) { return f.binary, nil }, config: client.Config{Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS).Transport}}
	if code := agent.app.run(t.Context(), []string{"join", w.invitation}); code != 0 {
		t.Fatal("join failed", code, agent.diagnostic.String())
	}
	options, err := client.Resolve(client.Options{}, base, agent.app.env)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(options.ClientKey), client.HostKeysFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("join did not write private host keys", info, err)
	}
	certificate, err := tls.LoadX509KeyPair(options.ClientCert, options.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	agent.app.config.Transport = f.client(f.config.Listen.HTTPS, certificate).Transport
	agent.out.Reset()
	return agent
}

func editInvitation(t *testing.T, path string, edit func(string) string) {
	t.Helper()
	raw, err := client.ReadPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	var invitation model.InvitationFile
	if err = json.Unmarshal(raw, &invitation); err != nil || !client.ValidHostKeys(invitation.KnownHosts) {
		t.Fatal("invitation carries no installation host keys", err)
	}
	invitation.KnownHosts = edit(invitation.KnownHosts)
	raw, _ = json.Marshal(invitation)
	if err = client.WritePrivate(path, raw); err != nil {
		t.Fatal(err)
	}
}

func privateDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func gitIn(t *testing.T, f *composedFixture, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), f.git.Binary, append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	output, err := cmd.CombinedOutput()
	return string(output), err
}
