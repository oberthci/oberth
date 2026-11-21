package main

import (
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"strconv"
	"testing"
)

func TestRepoAddAcceptsDisplayedSSHAddresses(t *testing.T) {
	for _, joined := range []bool{false, true} {
		for _, address := range []string{"ssh://git@forge.oberth.ci/team/widget.git", "git@forge.oberth.ci:team/widget.git"} {
			t.Run(strconv.FormatBool(joined)+address, func(t *testing.T) {
				b := fake.Bindings()
				sources := organizationSources()[:1]
				b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
				b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources, Repositories: []apicontract.Repository{{Repo: "one/team/widget"}}}}
				b.RepoAdd.Handler = &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "one/team/widget"}}
				command := urlCheckoutCommand(t, b, joined)
				command.address = address
				if code := command.run(t); code != 0 {
					t.Fatal(code, command.diagnostic.String())
				}
				if repo, err := runGit(t.Context(), command.app.directory, "config", "--get", "oberth.repository"); err != nil || repo != "one/team/widget" {
					t.Fatal(repo, err)
				}
			})
		}
	}
}

func TestSCPAddressesPreserveExplicitIPv6Host(t *testing.T) {
	u, err := parseRepositoryAddress("git@[::1]:team/widget.git")
	if err != nil || u.String() != "ssh://[::1]/team/widget.git" {
		t.Fatal(u, err)
	}
}
