package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func organizationSources() []apicontract.Upstream {
	return []apicontract.Upstream{
		{Name: "one", Org: "team", BaseURL: "ssh://forge.oberth.ci"},
		{Name: "two", Org: "elsewhere", BaseURL: "ssh://forge.oberth.ci"},
	}
}

func TestRepoAddOrganizationsDisambiguateHost(t *testing.T) {
	checkURLCheckout(t, urlCheckoutCase{sources: organizationSources(), repos: []string{"one/team/widget", "two/elsewhere/widget"}, want: "one/team/widget"})
	checkURLCheckout(t, urlCheckoutCase{sources: organizationSources(), repos: []string{"one/team/widget"}, explicit: "two", candidates: []string{"registered under another upstream", "one/team/widget", "Pass --upstream"}})
}

func TestRepoAddOutsideOrganizationRefusesBeforeMutation(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(map[bool]string{false: "administrator", true: "joined"}[joined], func(t *testing.T) {
			b := fake.Bindings()
			sources := organizationSources()[:1]
			b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
			b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources}}
			command := urlCheckoutCommand(t, b, joined)
			command.address = "ssh://forge.oberth.ci/elsewhere/widget.git"
			for _, flags := range [][]string{nil, {"--upstream", "one"}} {
				if code := command.run(t, flags...); code != 1 || !strings.Contains(command.diagnostic.String(), "outside the upstream organization") || strings.Contains(command.diagnostic.String(), "Administrator:") {
					t.Fatal(code, command.diagnostic.String())
				}
			}
			want := "--upstream <new-name>"
			if joined {
				want = "Ask an administrator"
			}
			if !strings.Contains(command.diagnostic.String(), want) {
				t.Fatal("wrong profile remedy", command.diagnostic.String())
			}
			assertNoRepositoryMutation(t, b)
			if _, err := runGit(t.Context(), command.app.directory, "remote", "get-url", "oberth"); err == nil {
				t.Fatal("refusal bound checkout")
			}
		})
	}
}
