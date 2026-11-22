package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
)

func serverTrustBindings() apicontract.Bindings {
	b := fake.Bindings()
	sources := organizationSources()[:1]
	b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources, Repositories: []apicontract.Repository{{Repo: "one/team/widget"}}}}
	b.RepoAdd.Handler = &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "one/team/widget"}}
	return b
}

// Right after install, and after join, repo add needs no flag: the selected
// profile holds the Oberth server's SSH host keys. A home known_hosts file is
// never consulted.
func TestRepoAddDefaultsToTheSelectedProfilesServerKeys(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(strconv.FormatBool(joined), func(t *testing.T) {
			command := urlCheckoutCommand(t, serverTrustBindings(), joined)
			command.app.env = func(string) string { return "" }
			if code := command.app.run(t.Context(), []string{"repo", "add", command.address}); code != 0 {
				t.Fatal(code, command.diagnostic.String())
			}
			config, err := os.ReadFile(filepath.Join(command.app.base, "install", "ssh.config"))
			if err != nil || !bytes.Contains(config, []byte(command.known)) {
				t.Fatal("checkout does not pin the profile's server keys", string(config), err)
			}
		})
	}
}

func TestRepoAddWithoutSavedServerKeysNamesTheFlag(t *testing.T) {
	b := serverTrustBindings()
	command := urlCheckoutCommand(t, b, false)
	command.app.env = func(string) string { return "" }
	path := filepath.Join(command.app.base, "install", "installation.json")
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
	before, err := os.ReadFile(filepath.Join(command.app.directory, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if code := command.app.run(t.Context(), []string{"repo", "add", command.address}); code != 1 {
		t.Fatal("repo add bound without the server's SSH host keys", code)
	}
	text := command.diagnostic.String()
	if !strings.Contains(text, "This profile saved no Oberth server SSH host keys.") || !strings.Contains(text, serverTrustRemedy) || strings.Contains(text, "known_hosts") {
		t.Fatal(text)
	}
	after, err := os.ReadFile(filepath.Join(command.app.directory, ".git", "config"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal changed checkout", err)
	}
	assertNoRepositoryMutation(t, b)
}

func TestRepoAddHelpSaysWhoseHostKeysEachFlagTakes(t *testing.T) {
	f := flowFlags("repo add")
	if usage := f.Lookup("known-hosts").Usage; !strings.HasPrefix(usage, "Oberth server's SSH host keys") || strings.Contains(usage, ".ssh") {
		t.Fatal(usage)
	}
	if usage := f.Lookup("expected-host-fingerprint").Usage; !strings.HasPrefix(usage, "The forge's SSH host-key fingerprint") {
		t.Fatal(usage)
	}
}
