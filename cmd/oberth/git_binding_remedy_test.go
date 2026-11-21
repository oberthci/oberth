package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestBindingRefusalNamesInvokedCommand(t *testing.T) {
	for _, command := range []string{"repo add", "join"} {
		for _, tc := range []struct{ mode, remedy string }{
			{"unavailable", "Unset remote.oberth-binding-preflight.url and remote.oberth-binding-preflight.pushurl, then rerun "},
			{"insteadOf", "Keep url.*.insteadOf compatible with the Oberth file address, then rerun "},
			{"pushInsteadOf", "Keep url.*.pushInsteadOf compatible with the Oberth file address, then rerun "},
			{"file-policy", "Allow file; check protocol.file.allow, protocol.allow, GIT_ALLOW_PROTOCOL, GIT_PROTOCOL_FROM_USER; then rerun "},
			{"inherited", "Unset remote.oberth.url and remote.oberth.pushurl outside this checkout, then rerun "},
		} {
			t.Run(command+tc.mode, func(t *testing.T) { checkInvokedBindingCommand(t, command, tc.mode, tc.remedy) })
		}
	}
}

func checkInvokedBindingCommand(t *testing.T, invoked, mode, stem string) {
	t.Helper()
	command, _ := bindingRepoCommand(t, invoked == "join")
	git := rebindGit(t, &command, "main-ssh")
	switch mode {
	case "inherited":
		installInheritedRemote(t, git, "global", "remote.oberth.pushurl")
	case "unavailable":
		installUnavailableBinding(t, git)
	default:
		installBindingPolicy(t, git, mode)
	}
	if invoked == "join" {
		session := flowSession{app: command.app, ctx: t.Context()}
		if bound, err := session.bindJoinedCheckout(filepath.Join(command.app.base, "install")); err == nil || bound {
			t.Fatal("join did not refuse incompatible settings", bound, err)
		} else {
			session.failed(err)
		}
	} else if code := command.run(t); code != 1 {
		t.Fatal("repo add did not refuse incompatible settings", code)
	}
	want := stem + invoked + "."
	if err := publicerr.CheckMessage(want, 120); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(strings.Fields(command.diagnostic.String()), " "), want) {
		t.Fatal("refusal omitted the whole command-specific remedy", want, command.diagnostic.String())
	}
}

func installUnavailableBinding(t *testing.T, git *bindingGit) {
	t.Helper()
	for _, address := range []string{"file:///old/one.git", "file:///old/two.git"} {
		if _, err := git.run(t.Context(), []string{"config", "--local", "--add", "remote.oberth-binding-preflight.pushurl", address}); err != nil {
			t.Fatal(err)
		}
	}
}
