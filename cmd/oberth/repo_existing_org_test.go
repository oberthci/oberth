package main

import (
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRepoAddRefusesAnExistingOrganizationBeforeRegistration(t *testing.T) {
	for _, tc := range []struct{ address, name string }{
		{"ssh://forge.oberth.ci/team/widget.git", "new-name"},
		{"ssh://other.oberth.ci/team/widget.git", ""},
		{"ssh://other.oberth.ci/team/widget.git", "new-name"},
	} {
		address, name := tc.address, tc.name
		t.Run(address+name, func(t *testing.T) {
			b := fake.Bindings()
			b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: organizationSources()[:1]}}
			command := urlCheckoutCommand(t, b, false)
			command.address = address
			path := filepath.Join(command.app.directory, ".git", "config")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"--upstream", name}
			if code := command.run(t, args...); code != 1 || !strings.Contains(command.diagnostic.String(), "one") {
				t.Fatal(code, command.diagnostic.String())
			}
			assertNoRepositoryMutation(t, b)
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("conflict changed checkout", err)
			}
		})
	}
}

func TestRepoAddOrganizationOnAnotherForgeHasCompleteRemedy(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(strconv.FormatBool(joined), func(t *testing.T) {
			b := fake.Bindings()
			sources := organizationSources()[:1]
			b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
			b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources}}
			command := urlCheckoutCommand(t, b, joined)
			command.address = "ssh://other.oberth.ci/team/widget.git"
			if code := command.run(t); code != 1 {
				t.Fatal("another forge accepted", code)
			}
			text := strings.Join(strings.Fields(command.diagnostic.String()), " ")
			for _, message := range []string{"Another upstream already has this organization name.", "Use a different directory name for this forge; each organization name has one upstream.", "Organization team: upstream one, ssh://forge.oberth.ci"} {
				if !strings.Contains(text, message) {
					t.Fatal("incomplete organization remedy", message, text)
				}
			}
			if strings.Contains(text, "select the matching upstream") {
				t.Fatal("impossible remedy retained", text)
			}
			assertNoRepositoryMutation(t, b)
		})
	}
}
