package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// What the Git SSH helper prints for a server that does not answer is read
// as a transport failure by the cache's classifier, and a pin refusal is not.
func TestGitSSHTransportFailureReachesTheClassifier(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	identity, _ := composeSSHKey(t, dir, client.UplinkPushKeyFile)
	_, host := composeSSHKey(t, dir, "host.key")
	known := filepath.Join(dir, "known_hosts")
	if err = os.WriteFile(known, []byte(knownhosts.Line([]string{"[127.0.0.1]:" + port}, host)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "ssh.config")
	if err = os.WriteFile(config, sshConfiguration(sshConnection{Host: "127.0.0.1", User: "git", Port: port, Key: identity, Hosts: known}), 0600); err != nil {
		t.Fatal(err)
	}
	app, _, diagnostic := fixture(t, fake.Bindings())
	args := []string{"--config", config, "git@127.0.0.1", "git-upload-pack 'origin/team/widget.git'"}
	if code := app.gitSSH(t.Context(), args); code != 1 || !gitssh.TransportFailure(diagnostic.String()) {
		t.Fatal("a server that did not answer was not read as a transport failure", code, diagnostic.String())
	}
	if err = os.WriteFile(known, []byte(knownhosts.Line([]string{"another-host"}, host)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostic.Reset()
	if code := app.gitSSH(t.Context(), args); code != 1 || gitssh.TransportFailure(diagnostic.String()) || !strings.Contains(diagnostic.String(), "No usable Git host key pin.") {
		t.Fatal("a pin refusal was read as a transport failure", code, diagnostic.String())
	}
}
