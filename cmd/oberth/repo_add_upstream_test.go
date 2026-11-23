//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestRepoAddNamesAndReusesExistingUpstream(t *testing.T) {
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
	var adds, registrations atomic.Int32
	original := command.app.config.Transport
	command.app.config.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(data))
		if bytes.Contains(data, []byte(`"name":"upstream_add"`)) {
			adds.Add(1)
		}
		if bytes.Contains(data, []byte(`"name":"repo_add"`)) {
			registrations.Add(1)
		}
		return original.RoundTrip(r)
	})
	flags := []string{"--upstream", "release-source", "--expected-host-fingerprint", ssh.FingerprintSHA256(f.forge.key)}
	if code := command.run(t, flags...); code != 0 {
		t.Fatal("explicit upstream could not connect", code, command.diagnostic.String())
	}
	if adds.Load() != 1 {
		t.Fatal("new upstream was not registered once", adds.Load())
	}
	assertCutoverUpstreamOutput(t, command, "release-source")
	command.app.directory = t.TempDir()
	if _, err := runGit(t.Context(), command.app.directory, "init"); err != nil {
		t.Fatal(err)
	}
	// Existing access must be reused even if creation-only trust flags differ.
	if code := command.run(t, "--upstream", "release-source", "--expected-host-fingerprint", "unused", "--repin"); code != 0 {
		t.Fatal("existing upstream was mutated instead of reused", code, command.diagnostic.String())
	}
	if adds.Load() != 1 {
		t.Fatal("existing upstream was reconfigured", adds.Load())
	}
	assertCutoverUpstreamOutput(t, command, "release-source")
	if repo, err := runGit(t.Context(), command.app.directory, "config", "--get", "oberth.repository"); err != nil || repo != "release-source/team/widget" {
		t.Fatal("wrong canonical checkout identity", repo, err)
	}
	assertReusedUpstreamURLRefusals(t, command, &registrations)
	if adds.Load() != 1 {
		t.Fatal("address refusals mutated upstreams")
	}
}

func assertReusedUpstreamURLRefusals(t *testing.T, command repoAddCommand, registrations *atomic.Int32) {
	t.Helper()
	originalAddress := command.address
	for _, change := range []string{"host", "port", "scheme", "base-path"} {
		u, _ := url.Parse(originalAddress)
		switch change {
		case "host":
			u.Host = "other.oberth.ci"
		case "port":
			u.Host = u.Hostname() + ":1"
		case "scheme":
			u.Scheme = "https"
		case "base-path":
			u.Path = "/another" + u.Path
		}
		// Another forge or account whose directory has the same name clashes with
		// the organization; another namespace on the same account is the
		// registered address mistyped.
		messages := [2]string{"Another upstream already has this organization name.", "Use a different directory name for this forge; each organization name has one upstream."}
		if change == "scheme" || change == "base-path" {
			messages = [2]string{"This organization uses release-source.", "Use its registered forge address; an organization has one upstream."}
		}
		t.Run(change, func(t *testing.T) {
			assertRefusedUpstreamURL(t, command, refusedUpstreamURL{u.String(), messages}, registrations)
		})
	}
}

// refusedUpstreamURL is a changed address and the reason and remedy it gets.
type refusedUpstreamURL struct {
	address  string
	messages [2]string
}

func assertRefusedUpstreamURL(t *testing.T, command repoAddCommand, refused refusedUpstreamURL, registrations *atomic.Int32) {
	t.Helper()
	address, messages := refused.address, refused.messages
	originalAddress := command.address
	command.address = address
	command.app.directory = t.TempDir()
	if _, err := runGit(t.Context(), command.app.directory, "init"); err != nil {
		t.Fatal(err)
	}
	before := registrations.Load()
	code := command.run(t, "--upstream", "release-source")
	text := strings.Join(strings.Fields(command.diagnostic.String()), " ")
	if code != 1 || !strings.Contains(text, messages[0]) || !strings.Contains(text, messages[1]) || !strings.Contains(text, originalAddress[:strings.LastIndex(originalAddress, "/team/")]) || !strings.Contains(text, address) {
		t.Fatal("different address not explained", code, command.diagnostic.String())
	}
	if registrations.Load() != before {
		t.Fatal("refusal mutated registrations")
	}
	if _, err := runGit(t.Context(), command.app.directory, "config", "--get", "oberth.repository"); err == nil {
		t.Fatal("refusal bound checkout")
	}
	if _, err := runGit(t.Context(), command.app.directory, "remote", "get-url", "oberth"); err == nil {
		t.Fatal("refusal wrote remote")
	}
}

func assertCutoverUpstreamOutput(t *testing.T, command repoAddCommand, name string) {
	t.Helper()
	for _, expected := range []string{"Upstream: " + name, "Step accounts and release subjects derive from this name.", "bound:\n" + name + "/team/widget"} {
		if !strings.Contains(command.out.String(), expected) {
			t.Fatal("upstream identity is not visible", command.out.String())
		}
	}
}
