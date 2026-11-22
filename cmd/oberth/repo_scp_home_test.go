package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An SCP-style address keeps Git's meaning in the stored ssh:// address: a
// relative path of an account other than git is relative to its home, an
// absolute one stays absolute, and git@ keeps the form hosted forges serve.
func TestSCPAddressesKeepGitsMeaning(t *testing.T) {
	for _, tc := range []struct{ address, stored, requested string }{
		{"oberth@192.168.86.247:repos/oberth-smoke.git", "ssh://oberth@192.168.86.247/~/repos/oberth-smoke.git", "git-upload-pack '~/repos/oberth-smoke.git'"},
		{"oberth@192.168.86.247:/home/oberth/repos/oberth-smoke.git", "ssh://oberth@192.168.86.247/home/oberth/repos/oberth-smoke.git", "git-upload-pack '/home/oberth/repos/oberth-smoke.git'"},
		{"git@github.com:org/repo.git", "ssh://github.com/org/repo.git", "git-upload-pack '/org/repo.git'"},
		{"forge.oberth.ci:team/widget.git", "ssh://forge.oberth.ci/team/widget.git", "git-upload-pack '/team/widget.git'"},
	} {
		t.Run(tc.address, func(t *testing.T) {
			u, err := parseRepositoryAddress(tc.address)
			if err != nil || u.String() != tc.stored {
				t.Fatal("stored address differs", u, err)
			}
			if got := gitSSHRequest(t, u.String()); got != tc.requested {
				t.Fatal("Git resolves the stored address elsewhere", got)
			}
		})
	}
}

// gitSSHRequest is the remote command Git itself asks SSH to run for address.
func gitSSHRequest(t *testing.T, address string) string {
	t.Helper()
	dir := t.TempDir()
	recorder := filepath.Join(dir, "ssh")
	record := filepath.Join(dir, "request")
	if err := os.WriteFile(recorder, []byte("#!/bin/sh\nfor last in \"$@\"; do :; done\nprintf '%s' \"$last\" > \""+record+"\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "git", "ls-remote", "--", address)
	command.Dir = dir
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH=" + recorder, "GIT_SSH_VARIANT=simple"}
	_ = command.Run()
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal("Git did not reach SSH", err)
	}
	return strings.TrimSpace(string(raw))
}
