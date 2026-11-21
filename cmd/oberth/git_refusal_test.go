package main

import (
	"github.com/oberthci/oberth/internal/client"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"golang.org/x/crypto/ssh"
)

func TestGitSSHRefusalKeepsPinAndPermissionReasons(t *testing.T) {
	dir := t.TempDir()
	identity, _ := composeSSHKey(t, dir, client.UplinkPushKeyFile)
	_, host := composeSSHKey(t, dir, "host.key")
	known := filepath.Join(dir, "known_hosts")
	pins := "another-host " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(host))) + "\n"
	if err := os.WriteFile(known, []byte(pins), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "ssh.config")
	text := sshConfiguration(sshConnection{Host: "oberth.local", User: "git", Port: "22", Key: identity, Hosts: known})
	if err := os.WriteFile(config, text, 0600); err != nil {
		t.Fatal(err)
	}
	app, _, diagnostic := fixture(t, fake.Bindings())
	args := []string{"--config", config, "git@oberth.local", "git-upload-pack 'origin/team/widget.git'"}
	if code := app.gitSSH(t.Context(), args); code != 1 || !strings.Contains(diagnostic.String(), "No usable Git host key pin.") {
		t.Fatal("pin reason was hidden", code, diagnostic.String())
	}
	diagnostic.Reset()
	if err := os.Chmod(config, 0666); err != nil {
		t.Fatal(err)
	}
	if code := app.gitSSH(t.Context(), args); code != 1 || !strings.Contains(diagnostic.String(), "file permissions are unsafe") {
		t.Fatal("permission reason was hidden", code, diagnostic.String())
	}
}
