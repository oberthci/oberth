package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/client"
)

// Join binds the checkout with the Oberth server's SSH host keys the
// invitation delivered; a home known_hosts file never stands in for them.
func TestJoinBindingNeverFallsBackToHomeKnownHosts(t *testing.T) {
	command := urlCheckoutCommand(t, fake.Bindings(), true)
	homeKnownHosts(t, &command)
	dir := filepath.Join(command.app.base, "install")
	clearProfileServerKeys(t, dir)
	if _, err := runGit(t.Context(), command.app.directory, "config", "oberth.repository", "origin/team/widget"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(command.app.directory, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	session := flowSession{app: command.app, ctx: t.Context()}
	bound, err := session.bindJoinedCheckout(dir)
	if err == nil || bound {
		t.Fatal("join bound the checkout with a home known_hosts file", bound)
	}
	session.failed(err)
	if text := command.diagnostic.String(); !strings.Contains(text, "This profile saved no Oberth server SSH host keys.") || !strings.Contains(text, serverTrustRemedy) {
		t.Fatal(text)
	}
	after, err := os.ReadFile(filepath.Join(command.app.directory, ".git", "config"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal changed checkout", err)
	}
}

// homeKnownHosts puts the server's keys into the person's own known_hosts,
// where a fallback would find them.
func homeKnownHosts(t *testing.T, command *repoAddCommand) {
	t.Helper()
	home := t.TempDir()
	raw, err := client.ReadPublic(command.known)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = client.WritePrivate(filepath.Join(home, ".ssh", "known_hosts"), raw); err != nil {
		t.Fatal(err)
	}
	command.app.env = func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	}
}

// clearProfileServerKeys removes the saved Oberth server SSH host keys from
// the profile in dir.
func clearProfileServerKeys(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "installation.json")
	raw, err := client.ReadPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	var profile client.Profile
	if err = json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	profile.KnownHosts = ""
	raw, _ = json.Marshal(profile)
	if err = client.WritePrivate(path, raw); err != nil {
		t.Fatal(err)
	}
}
