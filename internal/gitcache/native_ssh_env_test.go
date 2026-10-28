package gitcache

import (
	"strings"
	"testing"
)

func TestNativeSSHEnvironmentCannotBecomeShellCommand(t *testing.T) {
	cache := &Cache{env: map[string]string{"OBERTH_GIT_SSH_CONFIG": "/policy/config", "GIT_SSH": "/bin/oberth-git-ssh", "GIT_SSH_VARIANT": "simple", "GIT_SSH_COMMAND": "bad inherited command"}}
	environment := cache.commandEnv(map[string]string{"GIT_SSH_COMMAND": "bad extra command", "GIT_SSH_VARIANT": "ssh"})
	values := map[string]string{}
	for _, item := range environment {
		key, value, _ := strings.Cut(item, "=")
		values[key] = value
	}
	if _, present := values["GIT_SSH_COMMAND"]; present {
		t.Fatal("native transport reverted to shell command")
	}
	if values["GIT_SSH"] != "/bin/oberth-git-ssh" || values["GIT_SSH_VARIANT"] != "simple" {
		t.Fatal("native transport replaced")
	}
}
