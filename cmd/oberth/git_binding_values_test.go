package main

import (
	"os/exec"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestGitBindingUsesSingleNonemptyValues(t *testing.T) {
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	command := urlCheckoutCommand(t, fake.Bindings(), false)
	git, _ := bindingPolicyGit(t, &command, binary)
	for _, key := range []string{"url", "pushurl", "fetch", "uploadpack", "receivepack", "vcs"} {
		for _, value := range []string{"old-one", "old-two"} {
			if _, err := git.run(t.Context(), []string{"config", "--local", "--add", "remote.oberth." + key, value}); err != nil {
				t.Fatal(err)
			}
		}
	}
	session := flowSession{app: command.app, ctx: t.Context()}
	for range 2 {
		if err := session.bindGitRemote("origin/team/widget", "pack-command"); err != nil {
			t.Fatal(err)
		}
		if err := session.checkGitBinding("origin/team/widget", "repo add"); err != nil {
			t.Fatal(err)
		}
		assertNativeBindingAddresses(t, git)
		assertSingleBindingValues(t, git)
	}
}

func assertSingleBindingValues(t *testing.T, git *bindingGit) {
	t.Helper()
	for key, want := range map[string]string{"url": gitBindingAddress("origin/team/widget"), "fetch": "+refs/heads/*:refs/remotes/oberth/*", "uploadpack": "pack-command git-upload-pack", "receivepack": "pack-command git-receive-pack"} {
		got, err := git.run(t.Context(), []string{"config", "--local", "--get-all", "remote.oberth." + key})
		if err != nil || got != want {
			t.Fatal("binding has duplicate or empty values", key, got, err)
		}
	}
	for _, key := range []string{"pushurl", "vcs"} {
		if _, err := git.run(t.Context(), []string{"config", "--local", "--get-all", "remote.oberth." + key}); err == nil {
			t.Fatal("binding retained an unwanted list", key)
		}
	}
}
