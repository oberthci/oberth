package main

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/reponame"
)

func TestRepoAddSameForgeSpellingRefusesWithRegisteredAddress(t *testing.T) {
	for _, pair := range [][2]string{
		{"ssh://forge.oberth.ci", "ssh://FORGE.oberth.ci"},
		{"ssh://forge.oberth.ci", "ssh://forge.oberth.ci:22"},
		{"ssh://forge.oberth.ci", "ssh://forge.oberth.ci:"},
		{"ssh://forge.oberth.ci", "ssh://forge.oberth.ci."},
		{"ssh://forge.oberth.ci:22", "ssh://forge.oberth.ci"},
		{"https://forge.oberth.ci", "https://forge.oberth.ci:443"},
		{"http://forge.oberth.ci:80", "http://forge.oberth.ci"},
		{"ssh://[2001:db8::a]:22", "ssh://[2001:DB8::A]"},
	} {
		for _, joined := range []bool{false, true} {
			t.Run(pair[0]+pair[1]+strconv.FormatBool(joined), func(t *testing.T) {
				checkForgeSpellingRefusal(t, pair, joined)
			})
		}
	}
}

func checkForgeSpellingRefusal(t *testing.T, pair [2]string, joined bool) {
	t.Helper()
	sources := []apicontract.Upstream{{Name: "release-source", Org: "team", BaseURL: pair[0]}}
	b := fake.Bindings()
	b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources, Repositories: []apicontract.Repository{{Repo: "release-source/team/widget"}}}}
	command := urlCheckoutCommand(t, b, joined)
	command.address = pair[1] + "/team/widget.git"
	u, err := url.Parse(command.address)
	if err != nil || (reponame.Repository{Upstream: "release-source", Organization: "team", Name: "widget"}).MatchesURL(pair[0], u) {
		t.Fatal("address comparison admitted another spelling", err)
	}
	flags := [][]string{nil, {"--upstream", "release-source"}}
	if !joined {
		flags = append(flags, []string{"--upstream", "another"})
	}
	for _, args := range flags {
		if code := command.run(t, args...); code != 1 {
			t.Fatal("another spelling admitted", code)
		}
		text := command.diagnostic.String()
		if !strings.Contains(text, "same forge under another address spelling") || !strings.Contains(text, pair[0]) || strings.Contains(text, "Administrator:") {
			t.Fatal("wrong remedy", text)
		}
	}
	assertNoRepositoryMutation(t, b)
	if _, err := runGit(t.Context(), command.app.directory, "remote", "get-url", "oberth"); err == nil {
		t.Fatal("refusal bound checkout")
	}
}

func TestForgeSpellingDiagnosticDoesNotConflateOtherAddresses(t *testing.T) {
	for _, address := range []string{"ssh://other.oberth.ci", "https://forge.oberth.ci", "ssh://forge.oberth.ci:2222", "ssh://forge.oberth.ci.other"} {
		u, err := url.Parse(address)
		if err != nil || sameForgeSpelling("ssh://forge.oberth.ci", u) {
			t.Fatal("different forge described as spelling", address, err)
		}
	}
}
