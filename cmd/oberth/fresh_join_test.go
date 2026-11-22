//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/client"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestFreshJoinCarriesAdvertisedSSHToCheckout(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.config.Public.SSHPort = 43210
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	close(f.allowList)
	cancel, done := f.run(t)
	defer cancel()
	f.waitStarted(t, done)
	invitation := f.invitation(t, "fresh-agent")
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.base = t.TempDir()
	if err := os.Chmod(app.base, 0700); err != nil {
		t.Fatal(err)
	}
	app.directory = f.git.Work
	app.env = func(string) string { return "" }
	app.config.Now = f.deps.clock.Now
	app.config.Transport = f.client(f.config.Listen.HTTPS).Transport
	path := filepath.Join(t.TempDir(), "invitation.json")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(invitation)
	if err := client.WritePrivate(path, raw); err != nil {
		t.Fatal(err)
	}
	if code := app.run(t.Context(), []string{"join", path}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	options, err := client.Resolve(client.Options{}, app.base, app.env)
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(t.TempDir(), "known_hosts")
	_, hostKey := composeSSHKey(t, t.TempDir(), "server-pin")
	if err = os.WriteFile(known, []byte(knownhosts.Line([]string{"[oberth.local]:43210"}, hostKey)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	session := flowSession{app: app, ctx: t.Context(), resolved: options}
	connection, _, err := session.checkoutConnection(known)
	if err != nil || connection.SSHURL != "ssh://git@oberth.local:43210" {
		t.Fatal("fresh join cannot use advertised checkout endpoint", connection.SSHURL, err)
	}
	assertAdvertisedJoinedPort(t, filepath.Dir(options.ClientKey))
	if strings.Contains(out.String(), "next:\noberth repo add") || !strings.Contains(out.String(), "next:\noberth repo list") {
		t.Fatal("joined uplink was directed to an administrator operation", out.String())
	}
	assertJoinedWebView(t, out.String(), options.CAFile)
}

// Join says how to open the web view, naming the CA file it saved.
func assertJoinedWebView(t *testing.T, out, ca string) {
	t.Helper()
	if !strings.Contains(strings.Join(strings.Fields(out), " "), strings.Join(strings.Fields(joinedWebView(ca, []string{"origin"})), " ")) {
		t.Fatal("join does not say how to open the web view", out)
	}
	if _, err := client.ReadPublic(ca); err != nil {
		t.Fatal("join names a CA file it did not save", err)
	}
}

func assertAdvertisedJoinedPort(t *testing.T, dir string) {
	t.Helper()
	raw, err := client.ReadPublic(filepath.Join(dir, "installation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var profile client.Profile
	if err = json.Unmarshal(raw, &profile); err != nil || profile.SSHURL != "ssh://git@oberth.local:43210" {
		t.Fatal("profile lost authenticated SSH endpoint", profile.SSHURL, err)
	}
	raw, err = client.ReadPrivate(filepath.Join(dir, "ssh.config"))
	if err != nil || !strings.Contains(string(raw), "Port 43210\n") {
		t.Fatal("shim lost advertised SSH port", err)
	}
}

func TestMalformedJoinedSSHEndpointNeverPublishesOrSelects(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.config.Public.SSHPort = 43210
	f.prepareAdministrator(t)
	close(f.allowList)
	cancel, done := f.run(t)
	defer cancel()
	f.waitStarted(t, done)
	endpoints := []string{"ssh://git@other.local:22", "https://git@oberth.local:22", "ssh://git:password@oberth.local:22", "ssh://git@oberth.local:0", "ssh://git@oberth.local:999999", "ssh://other@oberth.local:22", "ssh://oberth.local:22", "ssh://git@oberth.local:22/path", "ssh://git@oberth.local:22?x=1", "ssh://git@oberth.local:22#part", "ssh://git@oberth.local:", "", "ssh://git@oberth.local:22/" + strings.Repeat("a", 1<<20), "ssh://git@oberth.local:" + strings.Repeat("0", 1<<20) + "22"}
	for index, endpoint := range endpoints {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			malformedJoinedEndpointCase(t, f, "invalid-"+string(rune('a'+index)), endpoint)
		})
	}
}

func malformedJoinedEndpointCase(t *testing.T, f *composedFixture, identity, endpoint string) {
	t.Helper()
	invitation := f.invitation(t, identity)
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	original := f.client(f.config.Listen.HTTPS).Transport
	changed := false
	config := client.Config{Now: f.deps.clock.Now, Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(r)
		if err != nil {
			return response, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, err
		}
		replacement, _ := json.Marshal(endpoint)
		updated := bytes.Replace(data, []byte(`"ssh://git@oberth.local:43210"`), replacement, 1)
		changed = !bytes.Equal(data, updated)
		response.Body = io.NopCloser(bytes.NewReader(updated))
		response.ContentLength = int64(len(updated))
		return response, nil
	})}
	dir := filepath.Join(base, "joined")
	_, err := client.Join(t.Context(), config, invitation, dir)
	if !changed {
		t.Fatal("fixture did not replace endpoint")
	}
	if err == nil || !strings.Contains(err.Error(), "invitation status is unknown") {
		t.Fatal("invalid handoff reported success", err)
	}
	for _, path := range []string{dir, filepath.Join(base, "current")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("invalid handoff published or selected", path, err)
		}
	}
}
