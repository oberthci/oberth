package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

// The address from a plain SSH server: its own account and a deep path.
const plainForgeURL = "ssh://oberth@192.168.86.247/home/oberth/repos/oberth-smoke.git"

func TestRepoAddRegistersThePlainSSHForgesUserAndPath(t *testing.T) {
	for _, address := range []string{plainForgeURL, "oberth@192.168.86.247:/home/oberth/repos/oberth-smoke.git", "oberth@192.168.86.247:home/oberth/repos/oberth-smoke.git"} {
		t.Run(address, func(t *testing.T) {
			b := fake.Bindings()
			b.RepoAdd.Handler = &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "192-168-86-247/repos/oberth-smoke"}}
			command := urlCheckoutCommand(t, b, false)
			command.address = address
			if code := command.run(t, "--expected-host-fingerprint", "SHA256:verified"); code != 0 {
				t.Fatal(code, command.diagnostic.String())
			}
			upstreams, _, calls := b.UpstreamAdd.Handler.(*fake.Handler[apicontract.UpstreamAddArgs, apicontract.Upstream]).Observed()
			want := apicontract.UpstreamAddArgs{Name: "192-168-86-247", BaseURL: "ssh://oberth@192.168.86.247/home/oberth/repos", CheckRepo: "repos/oberth-smoke", ExpectedHostFingerprint: "SHA256:verified"}
			if calls != 1 || upstreams[0] != want {
				t.Fatal("upstream lost the forge's user or namespace", upstreams)
			}
			repos, _, _ := b.RepoAdd.Handler.(*fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]).Observed()
			if len(repos) != 1 || repos[0].Repo != "192-168-86-247/repos/oberth-smoke" || !strings.Contains(command.out.String(), "192-168-86-247/repos/oberth-smoke") {
				t.Fatal("catalogue name differs", repos, command.out.String())
			}
		})
	}
}

func TestJoinedRepoAddBindsOnlyTheRegisteredForgeUser(t *testing.T) {
	sources := []apicontract.Upstream{{Name: "192-168-86-247", Org: "repos", BaseURL: "ssh://oberth@192.168.86.247/home/oberth/repos"}}
	for address, bound := range map[string]bool{
		plainForgeURL: true,
		"ssh://git@192.168.86.247/home/oberth/repos/oberth-smoke.git":   false,
		"ssh://other@192.168.86.247/home/oberth/repos/oberth-smoke.git": false,
		"ssh://oberth@192.168.86.247/repos/oberth-smoke.git":            false,
	} {
		t.Run(address, func(t *testing.T) {
			b := fake.Bindings()
			b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources, Repositories: []apicontract.Repository{{Repo: "192-168-86-247/repos/oberth-smoke"}}}}
			command := urlCheckoutCommand(t, b, true)
			command.address = address
			code := command.run(t)
			if bound != (code == 0) || bound != strings.Contains(command.out.String(), "192-168-86-247/repos/oberth-smoke") {
				t.Fatal("another account or path reached the registered repository", code, command.out.String(), command.diagnostic.String())
			}
		})
	}
}

func TestRepoAddDefaultUserKeepsTheStoredAddress(t *testing.T) {
	for address, want := range map[string]string{
		"git@forge.oberth.ci:team/widget.git":         "ssh://forge.oberth.ci/team/widget.git",
		"ssh://git@forge.oberth.ci//team/widget.git/": "ssh://forge.oberth.ci/team/widget.git",
		plainForgeURL: plainForgeURL,
	} {
		u, err := parseRepositoryAddress(address)
		if err != nil || u.String() != want {
			t.Fatal(address, u, err)
		}
	}
}
