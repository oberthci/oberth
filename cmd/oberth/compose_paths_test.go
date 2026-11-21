package main

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestGitShellParserPathsAreQuoted(t *testing.T) {
	for _, name := range []string{"plain", "unicode-é", "with space", "dollar$x", "semi;x", "tilde~x", "eq=x", "pct%x", "hash#x", "quote'x"} {
		t.Run(name, func(t *testing.T) {
			checkGitShellParserPath(t, name)
		})
	}
}
func checkGitShellParserPath(t *testing.T, name string) {
	t.Helper()
	app, _, _ := fixture(t, fake.Bindings())
	binary := filepath.Join(t.TempDir(), "oberth")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	app.executable = func() (string, error) { return binary, nil }
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	_, profile := joinAdministratorProfile(t, &app, "install")
	pin, err := os.ReadFile(profile.KnownHosts)
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, pin, 0600); err != nil {
		t.Fatal(err)
	}
	session := flowSession{app: app, ctx: context.Background()}
	command, err := session.pushCommand(dir, known, &url.URL{Host: "oberth.local:2222"}, filepath.Join(dir, "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), "sh", "-c", command+" git-receive-pack /origin/team/widget.git").Output()
	want := []string{"--config", filepath.Join(dir, "ssh.config"), "--pack", "git@oberth.local", "git-receive-pack", "/origin/team/widget.git"}
	if err != nil || !reflect.DeepEqual(strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00"), want) {
		t.Fatalf("quoted invocation: %q %v", output, err)
	}

}

func TestGitSSHCommandOverrideDoesNotWarnForPerRemoteBinding(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	app.env = func(name string) string {
		if name == "GIT_SSH_COMMAND" {
			return "ssh"
		}
		return ""
	}
	app.git = func(context.Context, []string) (string, error) { return "", nil }
	session := flowSession{app: app, ctx: t.Context()}
	if _, err := session.git("push"); err != nil {
		t.Fatal(err)
	}
	if diagnostic.Len() != 0 {
		t.Fatal("unrelated SSH configuration was discouraged", diagnostic.String())
	}
}
