package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitssh"
)

func TestNativeGitSSHEnvironmentUsesPathAndExistingPolicy(t *testing.T) {
	root := t.TempDir()
	options := serveOptions{upstreamKey: filepath.Join(root, "fallback key"), knownHosts: filepath.Join(root, "known hosts")}
	key := filepath.Join(root, "dedicated key")
	path := filepath.Join(root, "config")
	// Projected Secret files need not exist during bootstrap. No key bytes
	// are copied into the generated policy file.
	env, err := nativeGitSSHEnvironment([]app.SSHConfigEntry{{Host: "forge.example", Port: "30022", IdentityFile: key}}, options, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := env["GIT_SSH_COMMAND"]; ok {
		t.Fatal("shell-interpreted transport returned")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if env["GIT_SSH"] != filepath.Join(filepath.Dir(executable), "oberth-git-ssh") || env["GIT_SSH_VARIANT"] != "simple" || env[gitssh.ConfigEnvironment] != path {
		t.Fatalf("wrong native environment: %v", env)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Host forge.example\n", "Port 30022", "IdentityFile \"" + key + "\"", "Host * !forge.example\n", "StrictHostKeyChecking yes", "GlobalKnownHostsFile /dev/null", "UserKnownHostsFile \"" + options.knownHosts + "\""} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("lost policy %q", want)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions %v %v", info, err)
	}
}

func TestNativeGitSSHEnvironmentKeepsFallbackWhenNoPerHostKeys(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config")
	options := serveOptions{upstreamKey: filepath.Join(root, "key"), knownHosts: filepath.Join(root, "hosts")}
	if _, err := nativeGitSSHEnvironment(nil, options, path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Host *\n") || !strings.Contains(string(body), "IdentityFile \""+options.upstreamKey+"\"") {
		t.Fatal("fallback identity lost")
	}
}
