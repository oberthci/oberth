package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestRepoAddCredentialsRefusedBeforeCheckoutAndCalls(t *testing.T) {
	for _, joined := range []bool{false, true} {
		for _, address := range []string{"https://user:private-value@forge.oberth.ci/team/widget.git", "https://private-value@forge.oberth.ci/team/widget.git", "ssh://git:private-value@forge.oberth.ci/team/widget.git", "git:private-value@forge.oberth.ci:team/widget.git", "http://private-value@forge.oberth.ci/team/widget.git"} {
			t.Run(strconv.FormatBool(joined)+address, func(t *testing.T) {
				b := fake.Bindings()
				command := urlCheckoutCommand(t, b, joined)
				command.address = address
				command.app.directory = filepath.Join(t.TempDir(), "absent-checkout")
				if code := command.run(t, "--detail"); code != 1 || !strings.Contains(command.diagnostic.String(), "must not contain credentials") {
					t.Fatal(code, command.diagnostic.String())
				}
				if strings.Contains(command.diagnostic.String()+command.out.String(), "private-value") {
					t.Fatal("password was printed")
				}
				assertNoRepoAdministration(t, b)
				_, _, lists := b.RepoList.Handler.(*fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]).Observed()
				if lists != 0 {
					t.Fatal("credentials reached discovery")
				}
				if _, err := os.Stat(command.app.directory); !os.IsNotExist(err) {
					t.Fatal("refusal created checkout", err)
				}
			})
		}
	}
}

func TestRepositoryURLDiagnosticsRedactPasswords(t *testing.T) {
	u, err := url.Parse("https://user:private-value@other.oberth.ci/team/widget.git")
	if err != nil {
		t.Fatal(err)
	}
	source := apicontract.Upstream{Name: "one", Org: "team", BaseURL: "https://user:stored-value@forge.oberth.ci"}
	for _, problem := range []error{requireUpstreamURL(source, u, false), unavailableRepository(nil, u, "")} {
		app, _, diagnostic := fixture(t, fake.Bindings())
		app.failedDetail(problem, true)
		if strings.Contains(diagnostic.String(), "private-value") || strings.Contains(diagnostic.String(), "stored-value") || !strings.Contains(diagnostic.String(), "xxxxx") {
			t.Fatal("diagnostic did not redact passwords", diagnostic.String())
		}
	}
}

func TestRepoAddRefusesOtherSSHUsersBeforeCheckoutAndDiscovery(t *testing.T) {
	for _, joined := range []bool{false, true} {
		for _, address := range []string{"ssh://private-user@forge.oberth.ci/team/widget.git", "private-user@forge.oberth.ci:team/widget.git"} {
			t.Run(strconv.FormatBool(joined)+address, func(t *testing.T) { checkUnsupportedSSHUser(t, joined, address) })
		}
	}
}

func checkUnsupportedSSHUser(t *testing.T, joined bool, address string) {
	t.Helper()
	b := fake.Bindings()
	command := urlCheckoutCommand(t, b, joined)
	command.address = address
	command.app.directory = filepath.Join(t.TempDir(), "absent-checkout")
	if code := command.run(t, "--detail"); code != 1 {
		t.Fatal("unsupported SSH user accepted", code)
	}
	text := command.out.String() + command.diagnostic.String()
	for _, want := range []string{"Oberth reaches forges as git@.", "Use git@host in the SSH forge URL and retry."} {
		if !strings.Contains(text, want) {
			t.Fatal("missing remedy", text)
		}
	}
	if strings.Contains(text, "private-user") {
		t.Fatal("unsupported user echoed", text)
	}
	assertNoRepoAdministration(t, b)
	_, _, calls := b.RepoList.Handler.(*fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]).Observed()
	if calls != 0 {
		t.Fatal("unsupported user reached discovery")
	}
	if _, err := os.Stat(command.app.directory); !os.IsNotExist(err) {
		t.Fatal("refusal touched checkout", err)
	}
}
