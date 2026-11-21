//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestJoinedFreshCloneBindsRegisteredRepositoryAndPushes(t *testing.T) {
	t.Parallel()
	f, command, administration, commit := joinedCheckoutFixture(t)
	app, out, diagnostic := command.app, command.out, command.diagnostic
	known := command.known
	if code := app.run(t.Context(), []string{"repo", "list"}); code != 0 || !strings.Contains(out.String(), "origin/team/widget") {
		t.Fatal("scoped repository discovery", code, out.String(), diagnostic.String())
	}
	address := "ssh://" + f.forge.address + "/team/widget.git"
	refuseUnavailableCheckout(t, command)
	restoreMissingCheckout(t, f, command)
	diagnostic.Reset()
	// A joined caller neither confirms nor replaces the forge pin, even when
	// creation-only flags are supplied. Source retrieval must use the server pin.
	if code := app.run(t.Context(), []string{"repo", "add", address, "--known-hosts", known, "--expected-host-fingerprint", "unused", "--repin"}); code != 0 {
		t.Fatal("scoped registered checkout failed", code, diagnostic.String())
	}
	if code := app.run(t.Context(), []string{"repo", "add", address, "--known-hosts", known}); code != 0 {
		t.Fatal("binding cannot be repeated without forge confirmation", code, diagnostic.String())
	}
	if administration.Load() != 0 {
		t.Fatal("joined flow called administrator operations", administration.Load())
	}
	assertJoinedCheckoutBinding(t, command, f.config.Public.SSHPort)
	f.git.Git(t, app.directory, "push", "-o", "oberth.run=1", "oberth", "HEAD:refs/heads/joined-checkout")
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if run.Origin.Actor != "checkout-agent" || run.CommitSHA == nil || *run.CommitSHA != commit {
		t.Fatal("joined push did not start the expected native run", run.RunSummary)
	}
}

func joinedCheckoutFixture(t *testing.T) (*composedFixture, repoAddCommand, *atomic.Int32, string) {
	t.Helper()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareCommit(t)
	f.prepareAdministrator(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	f.config.Public.URL = "https://127.0.0.1"
	f.config.Public.SSHPort = listener.Addr().(*net.TCPAddr).Port
	listen := f.deps.listen
	f.deps.listen = func(ctx context.Context, network, address string) (net.Listener, error) {
		if address == f.config.Listen.SSH {
			f.ssh = listener
			return listener, nil
		}
		return listen(ctx, network, address)
	}
	close(f.allowList)
	cancel, done := f.run(t)
	t.Cleanup(cancel)
	f.waitStarted(t, done)
	invitation := checkoutInvitation(t, f)
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.base = t.TempDir()
	if err := os.Chmod(app.base, 0700); err != nil {
		t.Fatal(err)
	}
	app.env = func(string) string { return "" }
	app.executable = func() (string, error) { return f.binary, nil }
	app.config.Now = f.deps.clock.Now
	var administration atomic.Int32
	app.config.Transport = countCheckoutAdministration(f, &administration)
	app.directory = filepath.Join(t.TempDir(), "checkout")
	f.git.Git(t, filepath.Dir(app.directory), "clone", "--branch", "main", f.git.Bare, app.directory)
	invitationPath := filepath.Join(app.base, "invitation.json")
	raw, _ := json.Marshal(invitation)
	if err := client.WritePrivate(invitationPath, raw); err != nil {
		t.Fatal(err)
	}
	if code := app.run(t.Context(), []string{"join", invitationPath}); code != 0 {
		t.Fatal("fresh join", code, diagnostic.String())
	}
	installJoinedCheckoutTransport(t, f, &app, &administration)
	known := filepath.Join(app.base, "known_hosts")
	raw, err = client.ReadPrivate(f.config.Files.SSHHostKey)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WritePrivate(known, []byte(knownhosts.Line([]string{listener.Addr().String()}, signer.PublicKey())+"\n")); err != nil {
		t.Fatal(err)
	}

	return f, repoAddCommand{app, out, diagnostic, "ssh://" + f.forge.address + "/team/widget.git", known}, &administration, commit
}

func checkoutInvitation(t *testing.T, f *composedFixture) model.InvitationFile {
	t.Helper()
	transport := f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport.(*http.Transport)
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	remote, err := client.New(client.Config{Options: client.Options{URL: f.config.Public.URL}, Now: f.deps.clock.Now, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	path := filepath.Join(t.TempDir(), "invitation.json")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = remote.Invite(t.Context(), apicontract.UplinkInviteArgs{Identity: "checkout-agent", Scope: []string{"origin"}, ServerURL: f.config.Public.URL}, path, f.hostPins(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.ReadPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	var invitation model.InvitationFile
	if err := json.Unmarshal(raw, &invitation); err != nil {
		t.Fatal(err)
	}
	return invitation
}

func refuseUnavailableCheckout(t *testing.T, command repoAddCommand) {
	t.Helper()
	app, diagnostic, address, known := command.app, command.diagnostic, command.address, command.known
	for _, upstream := range []string{"outside", "origin"} {
		selected := address
		remedy := "Pass --upstream with the upstream name from a listed repository."
		if upstream == "origin" {
			selected = strings.TrimSuffix(address, "widget.git") + "missing.git"
			remedy = "Administrator:\noberth repo add '" + selected + "' --upstream 'origin'"
		}
		diagnostic.Reset()
		if code := app.run(t.Context(), []string{"repo", "add", selected, "--upstream", upstream, "--known-hosts", known}); code != 1 || !strings.Contains(diagnostic.String(), remedy) || !strings.Contains(diagnostic.String(), "origin") {
			t.Fatal("missing repository choice remedy", code, diagnostic.String())
		}
		if _, err := runGit(t.Context(), app.directory, "remote", "get-url", "oberth"); err == nil {
			t.Fatal("unavailable repository was bound")
		}
	}
}

func assertJoinedCheckoutBinding(t *testing.T, command repoAddCommand, port int) {
	t.Helper()
	app := command.app
	profile, err := runGit(t.Context(), app.directory, "config", "--get", "oberth.uplinkProfile")
	if err != nil || profile == "" {
		t.Fatal("checkout did not select joined MCP profile", profile, err)
	}
	remote, err := runGit(t.Context(), app.directory, "remote", "get-url", "oberth")
	if err != nil || remote != "file:///origin/team/widget.git" {
		t.Fatal("wrong push endpoint", remote, err)
	}
	config, err := client.ReadPrivate(filepath.Join(app.base, profile, "ssh.config"))
	if err != nil || !strings.Contains(string(config), "Port "+strconv.Itoa(port)+"\n") {
		t.Fatal("push configuration lost installation port", err)
	}
}

func countCheckoutAdministration(f *composedFixture, administration *atomic.Int32, certificates ...tls.Certificate) http.RoundTripper {
	original := f.client(f.config.Listen.HTTPS, certificates...).Transport.(*http.Transport)
	original.TLSClientConfig.ServerName = "127.0.0.1"
	return testTransport(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(data))
			for _, name := range []string{"upstream_list", "upstream_add", "repo_add", "repo_verify"} {
				if bytes.Contains(data, []byte(`"name":"`+name+`"`)) {
					administration.Add(1)
				}
			}
		}
		return original.RoundTrip(r)
	})
}

func installJoinedCheckoutTransport(t *testing.T, f *composedFixture, app *application, administration *atomic.Int32) {
	t.Helper()
	options, err := client.Resolve(client.Options{}, app.base, app.env)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.LoadX509KeyPair(options.ClientCert, options.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	app.config.Transport = countCheckoutAdministration(f, administration, certificate)
}

func restoreMissingCheckout(t *testing.T, f *composedFixture, joined repoAddCommand) {
	t.Helper()
	repo, err := f.owners.c.store.Repository(t.Context(), "origin/team/widget", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.owners.c.store.RemoveRepository(t.Context(), stateport.Action{Actor: "operator"}, repo.ID); err != nil {
		t.Fatal(err)
	}
	if code := joined.run(t); code != 1 {
		t.Fatal("unregistered repository accepted", code)
	}
	args := administratorArguments(t, joined.diagnostic.String())
	admin := newRepoAddCommand(t, f)
	admin.app.directory = t.TempDir()
	if _, err := runGit(t.Context(), admin.app.directory, "init"); err != nil {
		t.Fatal(err)
	}
	transport := admin.app.config.Transport.(*http.Transport)
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	administratorHome(t, &admin)
	if code := admin.app.run(t.Context(), args); code != 0 {
		t.Fatal("administrator remedy failed", code, admin.diagnostic.String())
	}
}
