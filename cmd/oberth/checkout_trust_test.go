package main

import (
	"bytes"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRepoAddChecksInstallationEndpointPinBeforeWriting(t *testing.T) {
	for _, joined := range []bool{false, true} {
		for _, host := range []string{"forge.oberth.ci", "[oberth.local]:22", "[oberth.local]:2222"} {
			t.Run(host+strconv.FormatBool(joined), func(t *testing.T) { checkInstallationEndpointPin(t, joined, host) })
		}
	}
}

func checkInstallationEndpointPin(t *testing.T, joined bool, host string) {
	t.Helper()
	b := fake.Bindings()
	source := apicontract.Upstream{Name: "origin", Org: "team", BaseURL: "ssh://forge.oberth.ci"}
	b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: []apicontract.Upstream{source}}}
	b.RepoAdd.Handler = &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "origin/team/widget"}}
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: []apicontract.Upstream{source}, Repositories: []apicontract.Repository{{Repo: "origin/team/widget"}}}}
	command := urlCheckoutCommand(t, b, joined)
	printed := command.known
	raw, err := client.ReadPublic(printed)
	if err != nil {
		t.Fatal(err)
	}
	_, _, key, _, _, err := ssh.ParseKnownHosts(raw)
	if err != nil {
		t.Fatal(err)
	}
	command.known = filepath.Join(t.TempDir(), "chosen_known_hosts")
	if err = os.WriteFile(command.known, []byte(knownhosts.Line([]string{host}, key)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(command.app.directory, ".git", "config")
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	code := command.run(t)
	if host == "[oberth.local]:2222" {
		if code != 0 {
			t.Fatal(code, command.diagnostic.String())
		}
		return
	}
	if code != 1 {
		t.Fatal("wrong endpoint pin accepted", code)
	}
	assertTrustRefusalUnchanged(t, command, b, before)
}

func assertTrustRefusalUnchanged(t *testing.T, command repoAddCommand, b apicontract.Bindings, before []byte) {
	t.Helper()

	text := command.diagnostic.String()
	if !strings.Contains(text, "Pass the SSH trust file printed by install.") || !strings.Contains(text, filepath.Join(command.app.base, "install", "known_hosts")) {
		t.Fatal(text)
	}
	after, err := os.ReadFile(filepath.Join(command.app.directory, ".git", "config"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal changed checkout", err)
	}
	for _, name := range []string{"ssh.config", "git-ssh"} {
		if _, err = os.Lstat(filepath.Join(command.app.base, "install", name)); !os.IsNotExist(err) {
			t.Fatal("refusal wrote profile", name, err)
		}
	}
	assertNoRepositoryMutation(t, b)
}
