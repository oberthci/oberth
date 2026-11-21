package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReleaseBuildRefusesMissingGitMetadataBeforeOtherWork(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := releaseBuildTree(t)
	writeFixtureCommand(t, filepath.Join(dir, "bin/git"), `exec "$REAL_GIT" "$@"`)
	output, err := buildScript(t, dir, map[string]string{"REAL_GIT": realGit, "GIT_CEILING_DIRECTORIES": filepath.Dir(dir), "VERSION": ""})
	assertMissingCheckout(t, dir, output, err)
}

func TestReleaseBuildRefusesUnavailableCommitDateBeforeOtherWork(t *testing.T) {
	dir := releaseBuildTree(t)
	writeFixtureCommand(t, filepath.Join(dir, "bin/git"), `if [ "$*" = 'rev-parse --verify HEAD' ]; then printf '%040d\n' 0; else echo native-detail >&2; exit 128; fi`)
	output, err := buildScript(t, dir, nil)
	assertMissingCheckout(t, dir, output, err)
}

func assertMissingCheckout(t *testing.T, dir string, output []byte, err error) {
	t.Helper()
	if err == nil || string(output) != "Release Git metadata is unavailable; build from a clean checkout.\n" {
		t.Fatal("missing checkout lacks a whole recovery instruction", err, string(output))
	}
	for _, name := range []string{"scratch", "build.log", "dist"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("checkout refusal did other work", name, err)
		}
	}
}
