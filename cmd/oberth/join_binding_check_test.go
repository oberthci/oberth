package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestJoinRebindingChecksGitBeforeSidecarWrites(t *testing.T) {
	for _, mode := range []string{"ordinary", "insteadOf", "pushInsteadOf", "file-policy", "protocol-policy", "allow-protocol", "empty-allow-protocol", "from-user"} {
		t.Run(mode, func(t *testing.T) { checkJoinBindingPolicy(t, mode) })
	}
}

func checkJoinBindingPolicy(t *testing.T, mode string) {
	t.Helper()
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	command := urlCheckoutCommand(t, fake.Bindings(), true)
	git, record := bindingPolicyGit(t, &command, binary)
	for _, setting := range [][2]string{{"oberth.repository", "origin/team/widget"}, {"remote.oberth.url", gitBindingAddress("origin/team/widget")}} {
		if _, err := git.run(t.Context(), []string{"config", setting[0], setting[1]}); err != nil {
			t.Fatal(err)
		}
	}
	installBindingPolicy(t, git, mode)
	before, err := os.ReadFile(filepath.Join(command.app.directory, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	session := flowSession{app: command.app, ctx: t.Context()}
	bound, err := session.bindJoinedCheckout(filepath.Join(command.app.base, "install"))
	if mode == "ordinary" {
		if err != nil || !bound {
			t.Fatal("ordinary join rebinding refused", bound, err)
		}
		assertNativeBindingAddresses(t, git)
		return
	}
	if err == nil || bound {
		t.Fatal("join accepted incompatible Git settings", bound, err)
	}
	session.failed(err)
	assertBindingPolicyRefusal(t, command, mode, 1)
	assertBindingUnwritten(t, command, before)
	if raw, err := os.ReadFile(record); err != nil || len(raw) != 0 {
		t.Fatal("join preflight invoked personal SSH", err)
	}
}
