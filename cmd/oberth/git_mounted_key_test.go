package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestGitSSHExternalKeyReadPermissionFloor(t *testing.T) {
	forge := composeForge(t, gittest.New(t))
	for _, projection := range []bool{false, true} {
		for _, server := range []bool{false, true} {
			for _, mode := range []os.FileMode{0600, 0440, 0644, 0666} {
				name := map[bool]string{false: "personal", true: "server"}[server] + "/" + map[bool]string{false: "file", true: "projection"}[projection] + "/" + mode.String()
				t.Run(name, func(t *testing.T) { externalGitKeyCase(t, forge, externalKeyMode{mode, server, projection}) })
			}
		}
	}
}

type externalKeyMode struct {
	mode               os.FileMode
	server, projection bool
}

type externalGitFiles struct{ config, key, host string }

func externalGitKeyFiles(t *testing.T, forge composedForge, test externalKeyMode) externalGitFiles {
	t.Helper()
	mount := t.TempDir()
	targetDir := filepath.Join(mount, ".epoch")
	if err := os.Mkdir(targetDir, 0700); err != nil {
		t.Fatal(err)
	}
	target, _ := composeSSHKey(t, targetDir, "key")
	if err := os.Chmod(target, test.mode); err != nil {
		t.Fatal(err)
	}
	key := target
	if test.projection {
		key = filepath.Join(mount, "key")
		if err := os.Symlink(".epoch/key", key); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	config, pins := filepath.Join(dir, "ssh.config"), filepath.Join(dir, "known_hosts")
	host, port, err := net.SplitHostPort(forge.address)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pins, []byte(knownhosts.Line([]string{forge.address}, forge.key)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data := sshConfiguration(sshConnection{Host: host, Port: port, User: "git", Key: key, Hosts: pins})
	if err = os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	return externalGitFiles{config, key, host}
}

func externalGitKeyCase(t *testing.T, forge composedForge, test externalKeyMode) {
	t.Helper()
	files := externalGitKeyFiles(t, forge, test)
	var out, diagnostic bytes.Buffer
	realNow := time.Now //nolint:forbidigo // Real SSH socket deadlines follow wall time.
	app := application{in: strings.NewReader("0000"), out: &out, diagnostic: &diagnostic, config: client.Config{Now: realNow}, env: func(name string) string {
		if test.server && name == gitssh.ConfigEnvironment {
			return files.config
		}
		return ""
	}}
	code := app.gitSSH(t.Context(), []string{"--config", files.config, "git@" + files.host, "git-upload-pack '/team/widget.git'"})
	accepted := test.mode == 0600 || test.mode == 0440
	if accepted {
		if code != 0 || out.Len() == 0 {
			t.Fatal("safe external key failed real SSH exchange", code, diagnostic.String())
		}
	} else if code != 1 || out.Len() != 0 {
		t.Fatal("unsafe external key reached Git", code, diagnostic.String())
	} else if test.server && (!strings.Contains(diagnostic.String(), "readable by others") || !strings.Contains(diagnostic.String(), files.key)) {
		t.Fatal("external-key refusal hid its repair or path", diagnostic.String())
	}
}

func TestPushOmitsTransportAliasWithoutChangingProtocol(t *testing.T) {
	forge := composeForge(t, gittest.New(t))
	files := externalGitKeyFiles(t, forge, externalKeyMode{mode: 0600})
	for _, server := range []bool{false, true} {
		for _, verb := range []string{"git-upload-pack", "git-receive-pack"} {
			var out, diagnostic bytes.Buffer
			realNow := time.Now //nolint:forbidigo // Real SSH socket deadlines follow wall time.
			app := application{in: strings.NewReader("0000"), out: &out, diagnostic: &diagnostic, config: client.Config{Now: realNow}, env: func(name string) string {
				if server && name == gitssh.ConfigEnvironment {
					return files.config
				}
				return ""
			}}
			code := app.gitSSH(t.Context(), []string{"--config", files.config, "git@" + files.host, verb + " '/team/widget.git'"})
			if code != 0 || out.Len() == 0 {
				t.Fatal("Git exchange failed", code, diagnostic.String())
			}
			explained := strings.Contains(diagnostic.String(), "Push transport: SSH. A file:/// URL is the remote transport alias.")
			if explained || strings.Contains(out.String(), "transport alias") {
				t.Fatal("wrong transport explanation", server, verb, diagnostic.String())
			}
		}
	}
}
