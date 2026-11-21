package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
)

func urlCheckoutCommand(t *testing.T, b apicontract.Bindings, joined bool) repoAddCommand {
	t.Helper()
	app, out, diagnostic := fixture(t, b)
	_, profile := joinAdministratorProfile(t, &app, "install")
	profile.Uplink = joined
	raw, _ := json.Marshal(profile)
	if err := client.WritePrivate(filepath.Join(app.base, "install", "installation.json"), raw); err != nil {
		t.Fatal(err)
	}
	app.directory = t.TempDir()
	if _, err := runGit(t.Context(), app.directory, "init"); err != nil {
		t.Fatal(err)
	}
	return repoAddCommand{app, out, diagnostic, "ssh://forge.oberth.ci/team/widget.git", profile.KnownHosts}
}

type urlCheckoutCase struct {
	name, address, explicit string
	sources                 []apicontract.Upstream
	repos                   []string
	want                    string
	candidates              []string
}

func TestRepoAddURLSelectsVisibleRepository(t *testing.T) {
	for _, tc := range []urlCheckoutCase{
		{name: "default-named-upstream", want: "release-source/team/widget"},
		{name: "explicit", explicit: "release-source", want: "release-source/team/widget"},
		{name: "other-host", address: "ssh://other.oberth.ci/team/widget.git"},
		{name: "other-port", address: "ssh://forge.oberth.ci:2222/team/widget.git"},
		{name: "other-base", address: "ssh://forge.oberth.ci/base/team/widget.git"},
		{name: "explicit-other-host", explicit: "release-source", address: "ssh://other.oberth.ci/team/widget.git"},
		{name: "not-visible", repos: []string{}},
		{name: "stored-base", address: "ssh://forge.oberth.ci/base/team/widget.git", sources: []apicontract.Upstream{{Name: "release-source", Org: "team", BaseURL: "ssh://forge.oberth.ci/base/team"}}, want: "release-source/team/widget"},
	} {
		t.Run(tc.name, func(t *testing.T) { checkURLCheckout(t, tc) })
	}
}

func checkURLCheckout(t *testing.T, tc urlCheckoutCase) {
	t.Helper()
	if tc.sources == nil {
		tc.sources = []apicontract.Upstream{{Name: "release-source", Org: "team", BaseURL: "ssh://forge.oberth.ci"}}
	}
	if tc.repos == nil {
		tc.repos = []string{"release-source/team/widget"}
	}
	page := apicontract.RepoPage{Upstreams: tc.sources}
	for _, name := range tc.repos {
		page.Repositories = append(page.Repositories, apicontract.Repository{Repo: name})
	}
	b := fake.Bindings()
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: page}
	command := urlCheckoutCommand(t, b, true)
	if tc.address != "" {
		command.address = tc.address
	}
	flags := []string{}
	if tc.explicit != "" {
		flags = append(flags, "--upstream", tc.explicit)
	}
	code := command.run(t, flags...)
	assertURLCheckout(t, command, tc, code)
	assertNoRepoAdministration(t, b)
}

func assertURLCheckout(t *testing.T, command repoAddCommand, tc urlCheckoutCase, code int) {
	t.Helper()
	if tc.want != "" {
		if code != 0 || !strings.Contains(command.out.String(), tc.want) {
			t.Fatal(code, command.out.String(), command.diagnostic.String())
		}
		got, err := runGit(t.Context(), command.app.directory, "config", "--get", "oberth.repository")
		if err != nil || got != tc.want {
			t.Fatal(got, err)
		}
	} else {
		if code != 1 {
			t.Fatal("unavailable URL accepted", code)
		}
		if _, err := runGit(t.Context(), command.app.directory, "remote", "get-url", "oberth"); err == nil {
			t.Fatal("refusal bound checkout")
		}
		for _, candidate := range tc.candidates {
			if !strings.Contains(command.diagnostic.String(), candidate) {
				t.Fatal("missing candidate", candidate, command.diagnostic.String())
			}
		}
	}

}

func assertNoRepoAdministration(t *testing.T, b apicontract.Bindings) {
	t.Helper()
	_, _, calls := b.UpstreamList.Handler.(*fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]).Observed()
	_, _, adds := b.UpstreamAdd.Handler.(*fake.Handler[apicontract.UpstreamAddArgs, apicontract.Upstream]).Observed()
	_, _, repos := b.RepoAdd.Handler.(*fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]).Observed()
	_, _, verifies := b.RepoVerify.Handler.(*fake.Handler[apicontract.RepoVerifyArgs, apicontract.Repository]).Observed()
	if calls+adds+repos+verifies != 0 {
		t.Fatal("joined caller attempted administration", calls, adds, repos, verifies)
	}
}

func TestMissingRepositoryAdministratorCommandExecutes(t *testing.T) {
	b := fake.Bindings()
	sources := []apicontract.Upstream{{Name: "release-source", Org: "team", BaseURL: "ssh://forge.oberth.ci"}}
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources}}
	joined := urlCheckoutCommand(t, b, true)
	if code := joined.run(t); code != 1 {
		t.Fatal("missing repository accepted", code)
	}
	args := administratorArguments(t, joined.diagnostic.String())
	want := []string{"repo", "add", joined.address, "--upstream", "release-source"}
	if !reflect.DeepEqual(args, want) {
		t.Fatal("wrong administrator command", args, want)
	}
	b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
	b.RepoAdd.Handler = &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "release-source/team/widget"}}
	admin := urlCheckoutCommand(t, b, false)
	administratorHome(t, &admin)
	if code := admin.app.run(t.Context(), args); code != 0 {
		t.Fatal("printed administrator command failed", code, admin.diagnostic.String())
	}
	_, _, adds := b.UpstreamAdd.Handler.(*fake.Handler[apicontract.UpstreamAddArgs, apicontract.Upstream]).Observed()
	if adds != 0 {
		t.Fatal("administrator command created another upstream")
	}
}

func administratorArguments(t *testing.T, text string) []string {
	t.Helper()
	_, command, ok := strings.Cut(text, "Administrator:\n")
	if !ok {
		t.Fatal("administrator command hidden", text)
	}
	command = strings.SplitN(command, "\n", 2)[0]
	shell := exec.CommandContext(t.Context(), "sh", "-c", "oberth() { printf '%s\\000' \"$@\"; }; "+command)
	raw, err := shell.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

func administratorHome(t *testing.T, command *repoAddCommand) {
	t.Helper()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := client.ReadPublic(command.known)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WritePrivate(filepath.Join(home, ".ssh", "known_hosts"), raw); err != nil {
		t.Fatal(err)
	}
	command.app.env = func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	}
}

func TestRepoAddExistingUpstreamURLChecksBeforeMutation(t *testing.T) {
	for _, address := range []string{"ssh://forge.oberth.ci/team/widget.git", "ssh://other.oberth.ci/team/widget.git", "ssh://forge.oberth.ci:22/team/widget.git", "https://forge.oberth.ci/team/widget.git", "ssh://forge.oberth.ci/base/team/widget.git"} {
		t.Run(address, func(t *testing.T) {
			b := fake.Bindings()
			b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: []apicontract.Upstream{{Name: "release-source", Org: "team", BaseURL: "ssh://forge.oberth.ci"}}}}
			add := &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "release-source/team/widget"}}
			b.RepoAdd.Handler = add
			command := urlCheckoutCommand(t, b, false)
			original := command.address
			command.address = address
			code := command.run(t, "--upstream", "release-source")
			_, _, calls := add.Observed()
			if address == original {
				if code != 0 || calls != 1 {
					t.Fatal("matching URL refused", code, command.diagnostic.String())
				}
			} else {
				if code != 1 || calls != 0 || !strings.Contains(command.diagnostic.String(), address) {
					t.Fatal("different address accepted or not explained", code, calls, command.diagnostic.String())
				}
				if _, err := runGit(t.Context(), command.app.directory, "remote", "get-url", "oberth"); err == nil {
					t.Fatal("mismatch bound checkout")
				}
			}
			_, _, creates := b.UpstreamAdd.Handler.(*fake.Handler[apicontract.UpstreamAddArgs, apicontract.Upstream]).Observed()
			if creates != 0 {
				t.Fatal("reuse mutated upstream")
			}
		})
	}
}

func TestRepoAddInvalidUpstreamNameIsHelpfulBeforeCalls(t *testing.T) {
	for _, joined := range []bool{false, true} {
		for _, name := range []string{"Bad Name", "UPPER", "a/b", "..", "release", strings.Repeat("a", 54)} {
			t.Run(name+strconv.FormatBool(joined), func(t *testing.T) {
				b := fake.Bindings()
				command := urlCheckoutCommand(t, b, joined)
				if code := command.run(t, "--upstream", name); code != 1 || !strings.Contains(command.diagnostic.String(), "upstream name") || strings.Contains(command.diagnostic.String(), "Operation failed") {
					t.Fatal("validation lost", code, command.diagnostic.String())
				}
				assertNoRepoAdministration(t, b)
				_, _, lists := b.RepoList.Handler.(*fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]).Observed()
				if lists != 0 {
					t.Fatal("invalid name reached repository lookup")
				}
			})
		}
	}
}

func TestRepositoryRebindingClearsPushURLsAndJoinedProfile(t *testing.T) {
	b := fake.Bindings()
	sources := []apicontract.Upstream{{Name: "release-source", Org: "team", BaseURL: "ssh://forge.oberth.ci"}}
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: sources, Repositories: []apicontract.Repository{{Repo: "release-source/team/widget"}}}}
	b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
	b.RepoAdd.Handler = &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "release-source/team/widget"}}
	joined := urlCheckoutCommand(t, b, true)
	if code := joined.run(t); code != 0 {
		t.Fatal(code, joined.diagnostic.String())
	}
	for _, value := range []string{"ssh://other.oberth.ci/one", "ssh://other.oberth.ci/two"} {
		if _, err := runGit(t.Context(), joined.app.directory, "config", "--add", "remote.oberth.pushurl", value); err != nil {
			t.Fatal(err)
		}
	}
	if code := joined.run(t); code != 0 {
		t.Fatal(code, joined.diagnostic.String())
	}
	if effective, err := runGit(t.Context(), joined.app.directory, "remote", "get-url", "--push", "oberth"); err != nil || effective != gitBindingAddress("release-source/team/widget") {
		t.Fatal("stale push URLs survived rebinding")
	}
	admin := urlCheckoutCommand(t, b, false)
	admin.app.directory = joined.app.directory
	if code := admin.run(t, "--upstream", "release-source"); code != 0 {
		t.Fatal(code, admin.diagnostic.String())
	}
	if _, err := runGit(t.Context(), joined.app.directory, "config", "--get", "oberth.uplinkProfile"); err == nil {
		t.Fatal("administrator binding kept joined identity")
	}
}

func TestDefaultAdministratorReusesStoredUpstreamAddress(t *testing.T) {
	for _, name := range []string{"forge-oberth-ci", "release-source"} {
		for _, refresh := range []bool{false, true} {
			t.Run(name+strconv.FormatBool(refresh), func(t *testing.T) { checkDefaultAdministratorUpstream(t, name, refresh) })
		}
	}
}

func checkDefaultAdministratorUpstream(t *testing.T, name string, refresh bool) {
	t.Helper()
	b := fake.Bindings()
	source := apicontract.Upstream{Name: name, Org: "team", BaseURL: "ssh://forge.oberth.ci/team"}
	b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: []apicontract.Upstream{source}}}
	b.RepoAdd.Handler = &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: name + "/team/widget"}}
	command := urlCheckoutCommand(t, b, false)
	flags := []string{}
	if refresh {
		flags = []string{"--expected-host-fingerprint", "verified", "--repin"}
	}
	if code := command.run(t, flags...); code != 0 {
		t.Fatal(code, command.diagnostic.String())
	}
	inputs, _, calls := b.UpstreamAdd.Handler.(*fake.Handler[apicontract.UpstreamAddArgs, apicontract.Upstream]).Observed()
	if !refresh && calls != 0 {
		t.Fatal("default connection recreated a registered upstream")
	}
	if refresh && (calls != 1 || inputs[0].Name != name || inputs[0].BaseURL != source.BaseURL || inputs[0].CheckRepo != "team/widget" || !inputs[0].Repin) {
		t.Fatal("trust refresh changed stored binding", inputs)
	}
	if !strings.Contains(command.out.String(), name+"/team/widget") {
		t.Fatal("wrong default binding", command.out.String())
	}
}
