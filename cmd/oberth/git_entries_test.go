package main

import (
	"bytes"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/gitssh"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitExecutableEntriesRefuseMalformedInvocations(t *testing.T) {
	for _, argv := range [][]string{{"/hooks/pre-receive", "extra"}, {"/hooks/reference-transaction"}, {"oberth", "git-ssh"}, {"/private/git-ssh", "--unexpected"}} {
		t.Run(filepath.Base(argv[0])+strings.Join(argv[1:], "-"), func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			app := application{in: strings.NewReader(""), out: &out, diagnostic: &diagnostic, env: func(string) string { return "" }}
			if code := app.runBinary(t.Context(), argv); code == 0 {
				t.Fatal("malformed Git entry succeeded", argv)
			}
			if out.Len() != 0 {
				t.Fatal("Git entry wrote CLI output into the Git protocol")
			}
		})
	}
}

func TestGitSSHRequiresExplicitConfigOrServerEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh.config")
	if err := os.WriteFile(path, []byte("unread config"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"absent", "explicit", "server"} {
		t.Run(mode, func(t *testing.T) { checkGitSSHConfigSource(t, path, mode) })
	}
}

func checkGitSSHConfigSource(t *testing.T, path, mode string) {
	t.Helper()
	app, _, diagnostic := fixture(t, fake.Bindings())
	args := []string{filepath.Join(filepath.Dir(path), "git-ssh")}
	want := "Git SSH file permissions are unsafe."
	if mode == "explicit" {
		args = append(args, "--config", path)
	}
	if mode == "server" {
		want = "Server state permissions are unsafe."
		app.env = func(key string) string {
			if key == gitssh.ConfigEnvironment {
				return path
			}
			return ""
		}
	}
	if mode == "absent" {
		want = "Git SSH configuration is invalid."
	}
	args = append(args, "git@oberth.local", "git-upload-pack '/origin/team/widget.git'")
	if code := app.runBinary(t.Context(), args); code != 1 || !strings.Contains(diagnostic.String(), want) {
		t.Fatal("implicit config or explicit source lost", code, diagnostic.String())
	}
}
