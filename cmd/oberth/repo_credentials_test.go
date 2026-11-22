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

// Each address would put an option into ssh or git, a second line into the
// SSH configuration, or a path outside the namespace an upstream reaches.
func TestRepoAddRefusesUnsafeForgeUsersAndPathsBeforeCheckoutAndDiscovery(t *testing.T) {
	for _, joined := range []bool{false, true} {
		for address, reason := range map[string]string{
			"ssh://-oProxyCommand=x@forge.oberth.ci/team/widget.git": "forge SSH user name is invalid",
			"ssh://a%0AProxyCommand%20x@forge.oberth.ci/team/w.git":  "forge SSH user name is invalid",
			"ssh://forge.oberth.ci/srv/../other/widget.git":          "forge URL path is invalid",
			"oberth@forge.oberth.ci:-upload/team/widget.git":         "forge URL path is invalid",
			"ssh://forge.oberth.ci/srv/%2e%2e/team/widget.git":       "forge URL path is invalid",
		} {
			t.Run(strconv.FormatBool(joined)+address, func(t *testing.T) { checkUnsafeForgeAddress(t, joined, address, reason) })
		}
	}
}

func checkUnsafeForgeAddress(t *testing.T, joined bool, address, reason string) {
	t.Helper()
	b := fake.Bindings()
	command := urlCheckoutCommand(t, b, joined)
	command.address = address
	command.app.directory = filepath.Join(t.TempDir(), "absent-checkout")
	if code := command.run(t, "--detail"); code != 1 {
		t.Fatal("unsafe forge address accepted", code)
	}
	if text := command.out.String() + command.diagnostic.String(); !strings.Contains(text, reason) {
		t.Fatal("refusal did not name the unsafe part", text)
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
