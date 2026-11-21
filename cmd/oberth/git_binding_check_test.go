package main

import (
	"context"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type bindingPolicyCase struct {
	binary, mode string
	joined       bool
}
type bindingGit struct {
	binary, directory, global string
	environment               []string
}

func (g *bindingGit) run(ctx context.Context, args []string) (string, error) {
	command := exec.CommandContext(ctx, g.binary, args...)
	command.Dir = g.directory
	command.Env = g.environment
	output, err := command.Output()
	if err != nil {
		return "", gitFailure(args, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func TestRepoAddChecksEffectiveGitBindingBeforeWrites(t *testing.T) {
	product, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{"/usr/bin/git", product} {
		if _, err := os.Stat(binary); err != nil {
			t.Fatal("Git executable missing", binary, err)
		}
		for _, joined := range []bool{false, true} {
			for _, mode := range []string{"ordinary", "insteadOf", "pushInsteadOf", "file-policy", "protocol-policy", "allow-protocol", "empty-allow-protocol", "from-user"} {
				t.Run(binary+mode+strconv.FormatBool(joined), func(t *testing.T) { checkEffectiveBinding(t, bindingPolicyCase{binary, mode, joined}) })
			}
		}
	}
}

func checkEffectiveBinding(t *testing.T, tc bindingPolicyCase) {
	t.Helper()
	command, b := bindingRepoCommand(t, tc.joined)
	git, record := bindingPolicyGit(t, &command, tc.binary)
	installBindingPolicy(t, git, tc.mode)
	config := filepath.Join(command.app.directory, ".git", "config")
	globalBefore, err := os.ReadFile(git.global)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	code := command.run(t, "--detail")
	if tc.mode == "ordinary" {
		if code != 0 {
			t.Fatal("ordinary binding refused", command.diagnostic.String())
		}
		assertNativeBindingAddresses(t, git)
		assertRepeatBinding(t, command)
	} else {
		assertBindingPolicyRefusal(t, command, tc.mode, code)
		assertBindingUnwritten(t, command, before)
		assertNoRepositoryMutation(t, b)
	}
	if globalAfter, err := os.ReadFile(git.global); err != nil || string(globalAfter) != string(globalBefore) {
		t.Fatal("personal Git configuration changed", err)
	}
	if raw, err := os.ReadFile(record); err != nil || len(raw) != 0 {
		t.Fatal("preflight invoked personal SSH", string(raw), err)
	}
}

func bindingPolicyGit(t *testing.T, command *repoAddCommand, binary string) (*bindingGit, string) {
	t.Helper()
	home := t.TempDir()
	record := filepath.Join(home, "ssh-record")
	if err := os.WriteFile(record, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ssh := filepath.Join(home, "personal-ssh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + shellArgument(record) + "\nexit 1\n"
	if err := os.WriteFile(ssh, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	environment := []string{"PATH=" + filepath.Dir(binary) + ":/usr/bin:/bin", "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"), "GIT_SSH_COMMAND=" + shellArgument(ssh)}
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0600); err != nil {
		t.Fatal(err)
	}
	git := &bindingGit{binary: binary, directory: command.app.directory, global: global, environment: environment}
	command.app.git = git.run
	version, err := git.run(t.Context(), []string{"--version"})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(version)
	return git, record
}

func installBindingPolicy(t *testing.T, git *bindingGit, mode string) {
	t.Helper()
	key, value := "", ""
	switch mode {
	case "ordinary":
		return
	case "insteadOf", "pushInsteadOf":
		key, value = "url.ssh://git@rewrite.oberth.ci/."+mode, "file:///"
	case "file-policy":
		key, value = "protocol.file.allow", "never"
	case "protocol-policy":
		key, value = "protocol.allow", "never"
	case "allow-protocol":
		git.environment = append(git.environment, "GIT_ALLOW_PROTOCOL=ssh:https")
	case "empty-allow-protocol":
		git.environment = append(git.environment, "GIT_ALLOW_PROTOCOL=")
	case "from-user":
		key, value = "protocol.file.allow", "user"
		git.environment = append(git.environment, "GIT_PROTOCOL_FROM_USER=0")
	}
	if key != "" {
		if _, err := git.run(t.Context(), []string{"config", "--global", key, value}); err != nil {
			t.Fatal(err)
		}
	}
}

func assertBindingPolicyRefusal(t *testing.T, command repoAddCommand, mode string, code int) {
	t.Helper()
	if code != 1 {
		t.Fatal("incompatible Git binding accepted", code)
	}
	text := strings.Join(strings.Fields(command.diagnostic.String()), " ")
	key, reason := "protocol.file.allow", "Git blocks the Oberth file transport."
	switch mode {
	case "insteadOf":
		key, reason = "url.*.insteadOf", "Git rewrites the Oberth fetch address."
	case "pushInsteadOf":
		key, reason = "url.*.pushInsteadOf", "Git rewrites the Oberth push address."
	case "protocol-policy":
		key = "protocol.allow"
	case "allow-protocol", "empty-allow-protocol":
		key = "GIT_ALLOW_PROTOCOL"
	case "from-user":
		key = "GIT_PROTOCOL_FROM_USER"
	}
	if !strings.Contains(text, reason) || !strings.Contains(text, key) {
		t.Fatal("incompatible setting not explained", text)
	}
}

func assertNativeBindingAddresses(t *testing.T, git *bindingGit) {
	t.Helper()
	for _, query := range [][]string{{"remote", "get-url", "--all", "oberth"}, {"remote", "get-url", "--push", "--all", "oberth"}} {
		address, err := git.run(t.Context(), query)
		if err != nil || address != gitBindingAddress("origin/team/widget") {
			t.Fatal("bound address differs from Git", address, err)
		}
	}
}

func assertBindingUnwritten(t *testing.T, command repoAddCommand, before []byte) {
	t.Helper()
	config := filepath.Join(command.app.directory, ".git", "config")
	after, err := os.ReadFile(config)
	if err != nil || string(after) != string(before) {
		t.Fatal("refusal wrote checkout", err)
	}
	for _, name := range []string{"ssh.config", "git-ssh"} {
		if _, err = os.Lstat(filepath.Join(command.app.base, "install", name)); !os.IsNotExist(err) {
			t.Fatal("refusal wrote profile", name, err)
		}
	}
}

func bindingRepoCommand(t *testing.T, joined bool) (repoAddCommand, apicontract.Bindings) {
	t.Helper()
	b := fake.Bindings()
	source := apicontract.Upstream{Name: "origin", Org: "team", BaseURL: "ssh://forge.oberth.ci"}
	b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: []apicontract.Upstream{source}}}
	b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Upstreams: []apicontract.Upstream{source}, Repositories: []apicontract.Repository{{Repo: "origin/team/widget"}}}}
	b.RepoAdd.Handler = &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "origin/team/widget"}}
	command := urlCheckoutCommand(t, b, joined)
	return command, b
}
