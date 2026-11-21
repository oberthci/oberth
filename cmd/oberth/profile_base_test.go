package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/oberthci/oberth/internal/client"
)

func TestProfileBaseUsesXDGOrHomeOnEverySystem(t *testing.T) {
	for _, xdg := range []string{"", "/config with space"} {
		base, err := profileBase(func(string) string { return xdg }, func() (string, error) { return "/home/person", nil })
		want := "/home/person/.config/oberth"
		if xdg != "" {
			want = filepath.Join(xdg, "oberth")
		}
		if err != nil || base != want {
			t.Fatal(base, err)
		}
	}
	missing := errors.New("home unavailable")
	for _, xdg := range []string{"", "/config"} {
		_, err := profileBase(func(string) string { return xdg }, func() (string, error) { return "", missing })
		if (xdg == "") != errors.Is(err, missing) {
			t.Fatal("home lookup failure was not handled", xdg, err)
		}
	}
}

func TestRepoAddPushThroughProfileWithSpaces(t *testing.T) {
	command, _ := bindingRepoCommand(t, false)
	relocateTestProfile(t, &command, "profile space ' $`é")
	bare := filepath.Join(t.TempDir(), "receiver.git")
	for _, args := range [][]string{{"init", "--bare", bare}, {"-c", "user.name=Test", "-c", "user.email=test@oberth.ci", "commit", "--allow-empty", "-m", "test"}} {
		if _, err := runGit(t.Context(), command.app.directory, args...); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(t.TempDir(), "oberth")
	script := "#!/bin/sh\n[ \"$1\" = --config ] && [ -f \"$2\" ] && [ \"$3\" = --pack ] && [ \"$5\" = git-receive-pack ] || exit 1\nexec git receive-pack " + shellArgument(bare) + "\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	command.app.executable = func() (string, error) { return binary, nil }
	if code := command.run(t); code != 0 {
		t.Fatal(code, command.diagnostic.String())
	}
	t.Logf("%s repo add exit=0\n%s", runtime.GOOS, command.out.String())
	if _, err := runGit(t.Context(), command.app.directory, "push", "oberth", "HEAD:refs/heads/profile-space"); err != nil {
		t.Fatal("push through quoted profile failed", err, errors.Unwrap(err))
	}
	want, _ := runGit(t.Context(), command.app.directory, "rev-parse", "HEAD")
	got, err := runGit(t.Context(), bare, "rev-parse", "refs/heads/profile-space")
	if err != nil || got != want {
		t.Fatal("push did not deliver the commit", got, want, err)
	}
	t.Log("push exit=0; receiver has the exact commit")
}

func relocateTestProfile(t *testing.T, command *repoAddCommand, name string) {
	t.Helper()
	base := filepath.Join(t.TempDir(), name)
	if err := os.Rename(command.app.base, base); err != nil {
		t.Fatal(err)
	}
	command.app.base = base
	dir := filepath.Join(base, "install")
	path := filepath.Join(dir, "installation.json")
	raw, err := client.ReadPublic(path)
	if err != nil {
		t.Fatal(err)
	}
	var profile client.Profile
	if err = json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	for _, value := range []*string{&profile.AdminKey, &profile.AdminCert, &profile.CAFile, &profile.KnownHosts} {
		if *value != "" {
			*value = filepath.Join(dir, filepath.Base(*value))
		}
	}
	command.known = profile.KnownHosts
	raw, _ = json.Marshal(profile)
	if err = client.WritePrivate(path, raw); err != nil {
		t.Fatal(err)
	}
}
