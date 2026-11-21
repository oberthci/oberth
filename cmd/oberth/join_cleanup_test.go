package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
)

func TestSuccessfulJoinRemovesSpentInvitation(t *testing.T) {
	s := newJoinScenario(t, "install")
	if s.app.run(t.Context(), []string{"join", s.invitation}) != 0 {
		t.Fatal(s.diagnostic.String())
	}
	if _, err := os.Lstat(s.invitation); !os.IsNotExist(err) {
		t.Fatal("spent invitation was retained", err)
	}
	if _, err := client.ReadPrivate(filepath.Join(s.app.base, s.uplink, "admin.key")); err != nil {
		t.Fatal("joined key was not retained", err)
	}
}

func TestBareCheckoutRefusalNamesWorkingTree(t *testing.T) {
	for _, action := range []string{"repo", "join"} {
		s := newJoinScenario(t, "install")
		s.app.git = nil
		s.app.directory = t.TempDir()
		if _, err := runGit(t.Context(), s.app.directory, "init", "--bare"); err != nil {
			t.Fatal(err)
		}
		args := []string{"join", s.invitation}
		if action == "repo" {
			args = []string{"repo", "add", "ssh://forge.oberth.ci/team/widget.git"}
		}
		if code := s.app.run(t.Context(), args); code != 1 || !strings.Contains(s.diagnostic.String(), "A bare repository has no working tree.") {
			t.Fatal("bare repository refusal was not specific", code, s.diagnostic.String())
		}
		if _, err := os.Lstat(s.invitation); err != nil {
			t.Fatal("bare repository spent invitation", err)
		}
	}
}

func TestCorrectingProfileAddressKeepsInvitationCredentials(t *testing.T) {
	command, _ := bindingRepoCommand(t, false)
	dir := filepath.Join(command.app.base, "install")
	path := filepath.Join(dir, "installation.json")
	raw, err := client.ReadPublic(path)
	if err != nil {
		t.Fatal(err)
	}
	var profile client.Profile
	if err = json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	profile.ServerURL = "https://public.oberth.ci"
	raw, _ = json.Marshal(profile)
	if err = client.WritePrivate(path, raw); err != nil {
		t.Fatal(err)
	}
	resolved, err := client.Resolve(client.Options{}, command.app.base, command.app.env)
	if err != nil || resolved.URL != profile.ServerURL || resolved.ClientKey != profile.AdminKey || resolved.ClientCert != profile.AdminCert || resolved.CAFile != profile.CAFile {
		t.Fatal("profile address remedy lost saved credentials", err)
	}
}
