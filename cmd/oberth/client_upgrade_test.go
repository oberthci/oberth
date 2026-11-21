package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientUpgradeRepairsShimAndKeepsCheckoutWorking(t *testing.T) {
	command, _ := bindingRepoCommand(t, false)
	stable, old, newer := upgradeBinaries(t)
	shim := filepath.Join(command.app.base, "install", "git-ssh")
	if err := os.Symlink(old, shim); err != nil {
		t.Fatal(err)
	}
	command.app.executable = func() (string, error) { return old, nil }
	if code := command.run(t); code != 0 {
		t.Fatal(code, command.diagnostic.String())
	}
	if command.diagnostic.String() != "Repaired the profile's Git helper.\n" {
		t.Fatal("repair was not explained once", command.diagnostic.String())
	}
	before, err := os.ReadFile(filepath.Join(command.app.directory, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(stable); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(newer, stable); err != nil {
		t.Fatal(err)
	}
	pack, err := runGit(t.Context(), command.app.directory, "config", "--get", "remote.oberth.receivepack")
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), "sh", "-c", pack).Output()
	if err != nil || string(output) != filepath.Base(newer) {
		t.Fatal("existing checkout did not follow upgrade", string(output), err)
	}
	command.app.executable = func() (string, error) { return newer, nil }
	if code := command.run(t); code != 0 || command.diagnostic.Len() != 0 {
		t.Fatal("upgraded client could not bind", code, command.diagnostic.String())
	}
	assertUpgradeBinding(t, command, before, stable)
}

func TestClientShimRefusesRegularFileWithClientRemedy(t *testing.T) {
	command, _ := bindingRepoCommand(t, false)
	shim := filepath.Join(command.app.base, "install", "git-ssh")
	if err := os.WriteFile(shim, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if command.run(t) != 1 || !strings.Contains(command.diagnostic.String(), "run oberth repo add") || strings.Contains(command.diagnostic.String(), "Server configuration") {
		t.Fatal("missing client remedy", command.diagnostic.String())
	}
	data, err := os.ReadFile(shim)
	if err != nil || string(data) != "keep" {
		t.Fatal("occupied helper was overwritten", err)
	}
}

func TestUpgradedClientRepairsVersionPinnedShim(t *testing.T) {
	command, _ := bindingRepoCommand(t, false)
	stable, old, newer := upgradeBinaries(t)
	if err := os.Remove(stable); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newer, stable); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(command.app.base, "install", "git-ssh")
	if err := os.Symlink(old, shim); err != nil {
		t.Fatal(err)
	}
	command.app.executable = func() (string, error) { return newer, nil }
	if code := command.run(t); code != 0 || command.diagnostic.String() != "Repaired the profile's Git helper.\n" {
		t.Fatal("upgraded client did not repair stale helper", code, command.diagnostic.String())
	}
	if target, err := os.Readlink(shim); err != nil || target != stable {
		t.Fatal("repaired helper did not select stable path", target, err)
	}
}

func upgradeBinaries(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	stable := filepath.Join(dir, "oberth")
	old := filepath.Join(dir, "oberth-v0.24.0")
	newer := filepath.Join(dir, "oberth-v0.24.1")
	for _, path := range []string{old, newer} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' "+shellArgument(filepath.Base(path))), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(old, stable); err != nil {
		t.Fatal(err)
	}
	return stable, old, newer
}

func assertUpgradeBinding(t *testing.T, command repoAddCommand, before []byte, stable string) {
	t.Helper()
	after, err := os.ReadFile(filepath.Join(command.app.directory, ".git", "config"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("upgrade changed checkout configuration", err)
	}
	command.out.Reset()
	if err = command.app.agentTools("uplink"); err != nil || !strings.Contains(command.out.String(), stable+" mcp") {
		t.Fatal("registration pinned a version", err, command.out.String())
	}
}
