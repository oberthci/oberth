package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

// On plain SSH forges the organization is a directory name, so a second forge
// or account with a directory of the same name clashes with the registered
// one; the refusal says so and what to change, before any registration.
func TestRepoAddNamesAnOrganizationClashWithAnotherForge(t *testing.T) {
	sources := []apicontract.Upstream{{Name: "h", Org: "repos", BaseURL: "ssh://oberth@h.oberth.ci/~/repos"}}
	for _, joined := range []bool{false, true} {
		for _, tc := range []struct {
			address string
			flags   []string
		}{
			{"ssh://oberth@other.oberth.ci/srv/repos/x.git", nil},
			{"ssh://oberth@other.oberth.ci/srv/repos/x.git", []string{"--upstream", "h2"}},
			{"ssh://alice@h.oberth.ci/home/alice/repos/x.git", nil},
		} {
			t.Run(strconv.FormatBool(joined)+tc.address+strings.Join(tc.flags, ""), func(t *testing.T) {
				b := fake.Bindings()
				b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
				b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources}}
				command := urlCheckoutCommand(t, b, joined)
				command.address = tc.address
				if code := command.run(t, tc.flags...); code != 1 {
					t.Fatal("clashing organization accepted", code)
				}
				text := strings.Join(strings.Fields(command.diagnostic.String()), " ")
				for _, want := range []string{"Another upstream already has this organization name.", "Use a different directory name for this forge; each organization name has one upstream.", "Organization repos: upstream h, ssh://oberth@h.oberth.ci/~/repos"} {
					if !strings.Contains(text, want) {
						t.Fatal("clash not explained", want, text)
					}
				}
				assertNoRepositoryMutation(t, b)
			})
		}
	}
}

func TestRepoAddSameAccountOtherNamespaceKeepsTheRegisteredAddressRemedy(t *testing.T) {
	b := fake.Bindings()
	sources := []apicontract.Upstream{{Name: "h", Org: "repos", BaseURL: "ssh://oberth@h.oberth.ci/~/repos"}}
	b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
	command := urlCheckoutCommand(t, b, false)
	command.address = "ssh://oberth@h.oberth.ci/srv/repos/x.git"
	if code := command.run(t); code != 1 || !strings.Contains(command.diagnostic.String(), "This organization uses h.") {
		t.Fatal("a mistyped namespace of the same account lost its remedy", code, command.diagnostic.String())
	}
	assertNoRepositoryMutation(t, b)
}
