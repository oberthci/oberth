package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

type recordedSSH struct {
	directory, program, record, home string
	environment                      []string
}

func recordSSH(t *testing.T, directory string) recordedSSH {
	t.Helper()
	home := t.TempDir()
	program, record := filepath.Join(home, "ssh"), filepath.Join(home, "invocations")
	script := "#!/bin/sh\nprintf 'HOME=%s\\000AGENT=%s\\000COMMAND=%s\\000' \"$HOME\" \"$SSH_AUTH_SOCK\" \"$GIT_SSH_COMMAND\" >> \"$SSH_RECORD\"\nprintf '%s\\000' \"$@\" >> \"$SSH_RECORD\"\nprintf '0000'\ncat >/dev/null\n"
	if err := os.WriteFile(program, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return recordedSSH{directory: directory, program: program, record: record, home: home, environment: []string{
		"PATH=" + home + ":" + os.Getenv("PATH"), "HOME=" + home, "SSH_AUTH_SOCK=" + filepath.Join(home, "agent"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "SSH_RECORD=" + record,
	}}
}

func (s recordedSSH) git(ctx context.Context, args []string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir, command.Env = s.directory, s.environment
	data, err := command.Output()
	if err != nil {
		return "", gitFailure(args, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func (s recordedSSH) invocation(t *testing.T) []string {
	t.Helper()
	if err := os.WriteFile(s.record, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.git(t.Context(), []string{"ls-remote", "other"}); err != nil {
		t.Fatal(err, errors.Unwrap(err))
	}
	data, err := os.ReadFile(s.record)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), s.program, "$FAKE_SSH")
	text = strings.ReplaceAll(text, s.home, "$HOME")
	return strings.Split(strings.TrimSuffix(text, "\x00"), "\x00")
}

func TestRepoAddPreservesOtherSSHInvocations(t *testing.T) {
	for _, name := range []string{"port-22", "port-2222", "core-command", "environment-command"} {
		t.Run(name, func(t *testing.T) { checkOtherSSHInvocation(t, name) })
	}
}

func checkOtherSSHInvocation(t *testing.T, name string) {
	t.Helper()
	b := fake.Bindings()
	sources := []apicontract.Upstream{{Name: "origin", Org: "team", BaseURL: "ssh://forge.oberth.ci"}}
	b.UpstreamList.Handler = &fake.Handler[apicontract.UpstreamListArgs, apicontract.UpstreamPage]{Value: apicontract.UpstreamPage{Upstreams: sources}}
	b.RepoAdd.Handler = &fake.Handler[apicontract.RepoAddArgs, apicontract.Repository]{Value: apicontract.Repository{Repo: "origin/team/widget"}}
	command := urlCheckoutCommand(t, b, false)
	ssh := recordSSH(t, command.app.directory)
	address := "ssh://git@forge-other.oberth.ci/team/widget.git"
	if name != "port-22" {
		address = "ssh://git@forge-other.oberth.ci:2222/team/widget.git"
	}
	configureOtherSSH(t, &ssh, name, address)
	command.app.git = ssh.git
	configuration, err := ssh.git(t.Context(), []string{"config", "--null", "--get-regexp", "^(core\\.|ssh\\.|remote\\.other\\.)"})
	if err != nil {
		t.Fatal(err)
	}
	before := ssh.invocation(t)
	if code := command.run(t, "--upstream", "origin"); code != 0 {
		t.Fatal("binding failed", code, command.diagnostic.String())
	}
	after := ssh.invocation(t)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("other remote's invocation changed", before, after)
	}
	unchanged, err := ssh.git(t.Context(), []string{"config", "--null", "--get-regexp", "^(core\\.|ssh\\.|remote\\.other\\.)"})
	if err != nil || unchanged != configuration {
		t.Fatal("unrelated Git settings changed", err)
	}
	assertRecordedSSH(t, name, before)
	assertRepeatBinding(t, command)
}

func configureOtherSSH(t *testing.T, ssh *recordedSSH, name, address string) {
	t.Helper()
	if name == "environment-command" {
		ssh.environment = append(ssh.environment, "GIT_SSH_COMMAND="+ssh.program+" --environment-flag")
	}
	for _, args := range [][]string{
		{"remote", "add", "other", address},
		{"config", "ssh.variant", "ssh"},
	} {
		if _, err := ssh.git(t.Context(), args); err != nil {
			t.Fatal(err)
		}
	}
	if name == "core-command" || name == "environment-command" {
		if _, err := ssh.git(t.Context(), []string{"config", "core.sshCommand", ssh.program + " --person-flag 'keep exact bytes'"}); err != nil {
			t.Fatal(err)
		}
	}
}

func assertRecordedSSH(t *testing.T, name string, got []string) {
	t.Helper()
	want := []string{"HOME=$HOME", "AGENT=$HOME/agent", "COMMAND="}
	switch name {
	case "environment-command":
		want[2] += "$FAKE_SSH --environment-flag"
		want = append(want, "--environment-flag")
	case "core-command":
		want = append(want, "--person-flag", "keep exact bytes")
	}
	want = append(want, "-o", "SendEnv=GIT_PROTOCOL")
	if name != "port-22" {
		want = append(want, "-p", "2222")
	}
	want = append(want, "git@forge-other.oberth.ci", "git-upload-pack '/team/widget.git'")
	if !reflect.DeepEqual(got, want) {
		t.Fatal("native SSH invocation differs from recorded contract", got, want)
	}
	t.Logf("unchanged SSH invocation: %q", got)
}

func assertRepeatBinding(t *testing.T, command repoAddCommand) {
	t.Helper()
	path := filepath.Join(command.app.directory, ".git", "config")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code := command.run(t, "--upstream", "origin"); code != 0 {
		t.Fatal("repeated binding failed", code, command.diagnostic.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("repeated binding changed Git configuration", err)
	}
}

func TestPackSSHArgumentsAdmitOnlyNativePackServices(t *testing.T) {
	for _, service := range []string{"git-upload-pack", "git-receive-pack"} {
		args, err := packSSHArguments([]string{"git@oberth.ci", service, "/origin/team/widget.git"})
		if err != nil || !reflect.DeepEqual(args, []string{"git@oberth.ci", service + " '/origin/team/widget.git'"}) {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{
		{"git@oberth.ci", "git-upload-archive", "/origin/team/widget.git"},
		{"git@oberth.ci", "git-receive-pack", "/other/origin/team/widget.git"},
		{"git@oberth.ci", "git-upload-pack", "-option"},
		{"git@oberth.ci", "git-upload-pack"},
	} {
		if _, err := packSSHArguments(args); err == nil {
			t.Fatal("invalid pack invocation accepted", args)
		}
	}
}
