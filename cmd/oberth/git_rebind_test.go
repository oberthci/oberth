package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestRepoAddRebindsExistingLocalAddresses(t *testing.T) {
	for _, joined := range []bool{false, true} {
		for _, shape := range []string{"main-ssh", "other-repository", "stale-pushurl"} {
			t.Run(shape+strconv.FormatBool(joined), func(t *testing.T) {
				command, _ := bindingRepoCommand(t, joined)
				git := rebindGit(t, &command, shape)
				if shape == "other-repository" {
					if _, err := git.run(t.Context(), []string{"config", "oberth.repository", "other/team/old"}); err != nil {
						t.Fatal(err)
					}
				}
				if code := command.run(t); code != 0 {
					t.Fatal("rebind refused", command.diagnostic.String())
				}
				assertReboundRemote(t, git)
				assertRepeatBinding(t, command)
			})
		}
	}
}

func TestJoinRebindsExistingLocalAddresses(t *testing.T) {
	for _, shape := range []string{"main-ssh", "other-repository", "stale-pushurl"} {
		t.Run(shape, func(t *testing.T) {
			command, _ := bindingRepoCommand(t, true)
			git := rebindGit(t, &command, shape)
			session := flowSession{app: command.app, ctx: t.Context()}
			for range 2 {
				bound, err := session.bindJoinedCheckout(filepath.Join(command.app.base, "install"))
				if err != nil || !bound {
					t.Fatal("join rebind refused", bound, err)
				}
				assertReboundRemote(t, git)
			}
		})
	}
}

func rebindGit(t *testing.T, command *repoAddCommand, shape string) *bindingGit {
	t.Helper()
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, _ := bindingPolicyGit(t, command, binary)
	address := gitBindingAddress("origin/team/widget")
	switch shape {
	case "main-ssh":
		address = "ssh://git@oberth.local:2222/origin/team/widget.git"
	case "other-repository":
		address = gitBindingAddress("other/team/old")
	}
	settings := [][2]string{{"oberth.repository", "origin/team/widget"}, {"remote.oberth.url", address}}
	if shape == "stale-pushurl" {
		settings = append(settings, [2]string{"remote.oberth.pushurl", "ssh://git@other.oberth.ci/old.git"})
	}
	for _, setting := range settings {
		if _, err := git.run(t.Context(), []string{"config", "--local", setting[0], setting[1]}); err != nil {
			t.Fatal(err)
		}
	}
	return git
}

func TestRepoAddRefusesInheritedRemoteAddressesBeforeWrites(t *testing.T) {
	for _, scope := range []string{"global", "system", "command", "worktree"} {
		for _, key := range []string{"url", "pushurl"} {
			t.Run(scope+key, func(t *testing.T) { checkInheritedRemote(t, scope, key) })
		}
	}
}

func checkInheritedRemote(t *testing.T, scope, key string) {
	t.Helper()
	command, bindings := bindingRepoCommand(t, false)
	git := rebindGit(t, &command, "main-ssh")
	installInheritedRemote(t, git, scope, "remote.oberth."+key)
	config := filepath.Join(command.app.directory, ".git", "config")
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if code := command.run(t); code != 1 {
		t.Fatal("inherited address accepted", code)
	}
	assertBindingUnwritten(t, command, before)
	assertNoRepositoryMutation(t, bindings)
	for _, text := range []string{"Git inherits Oberth remote addresses.", "remote.oberth.url", "remote.oberth.pushurl"} {
		if !strings.Contains(command.diagnostic.String(), text) {
			t.Fatal("inherited scope has no remedy", command.diagnostic.String())
		}
	}
}

func installInheritedRemote(t *testing.T, git *bindingGit, scope, key string) {
	t.Helper()
	value := "ssh://git@inherited.oberth.ci/old.git\nwhole value"
	if scope == "command" {
		git.environment = append(git.environment, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0="+key, "GIT_CONFIG_VALUE_0="+value)
		return
	}
	args := []string{"config", "--" + scope, key, value}
	if scope == "system" {
		git.environment = slices.DeleteFunc(git.environment, func(value string) bool { return strings.HasPrefix(value, "GIT_CONFIG_NOSYSTEM=") })
		git.environment = append(git.environment, "GIT_CONFIG_SYSTEM="+filepath.Join(filepath.Dir(git.global), "system"))
	}
	if scope == "worktree" {
		if _, err := git.run(t.Context(), []string{"config", "extensions.worktreeConfig", "true"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git.run(t.Context(), args); err != nil {
		t.Fatal(err)
	}
}

func assertReboundRemote(t *testing.T, git *bindingGit) {
	t.Helper()
	assertNativeBindingAddresses(t, git)
	if names, err := git.run(t.Context(), []string{"remote"}); err != nil || names != "oberth" {
		t.Fatal("preflight persisted another remote", names, err)
	}
}
