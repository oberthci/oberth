package main

import (
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRepoAddWrongUpstreamNamesTheMatchForAnUnregisteredRepository(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(strconv.FormatBool(joined), func(t *testing.T) {
			b := fake.Bindings()
			sources := organizationSources()
			b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
			b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources}}
			command := urlCheckoutCommand(t, b, joined)
			if code := command.run(t, "--upstream", "two"); code != 1 || !strings.Contains(command.diagnostic.String(), "Pass --upstream one and retry.") {
				t.Fatal(code, command.diagnostic.String())
			}
			assertNoRepositoryMutation(t, b)
		})
	}
}

func TestRepoAddMalformedAddressHasAnAddressRemedyAndNoEcho(t *testing.T) {
	b := fake.Bindings()
	command := urlCheckoutCommand(t, b, false)
	command.app.directory = filepath.Join(t.TempDir(), "absent")
	command.address = "ssh://git:private-marker@forge.oberth.ci:bad/team/widget.git"
	if code := command.run(t, "--detail"); code != 1 {
		t.Fatal(code)
	}
	text := command.diagnostic.String()
	if !strings.Contains(text, "Repository address is invalid.") || !strings.Contains(text, "user@host:path/repository.git") || strings.Contains(text, "private-marker") || strings.Contains(text, "regular file") {
		t.Fatal(text)
	}
	assertNoRepoAdministration(t, b)
}

func TestRepoAddPrefixedURLNamesItsOwnOrganizationUpstream(t *testing.T) {
	sources := organizationSources()
	sources[0], sources[1] = sources[1], sources[0]
	b := fake.Bindings()
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources}}
	command := urlCheckoutCommand(t, b, true)
	command.address = "ssh://forge.oberth.ci/prefix/team/widget.git"
	if code := command.run(t); code != 1 || !strings.Contains(command.diagnostic.String(), "Upstream one:") || strings.Contains(command.diagnostic.String(), "Upstream two:") {
		t.Fatal(code, command.diagnostic.String())
	}
	assertNoRepoAdministration(t, b)
}
