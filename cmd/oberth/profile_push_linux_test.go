//go:build linux

// These tests exercise the Linux server and Git receive path.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestProfileWithSpacesPushesThroughSSH(t *testing.T) {
	f, command, _, _ := joinedCheckoutFixture(t)
	base := filepath.Join(t.TempDir(), "profile with space ' $`é")
	if err := os.Rename(command.app.base, base); err != nil {
		t.Fatal(err)
	}
	command.known = filepath.Join(base, filepath.Base(command.known))
	command.app.base = base
	if code := command.run(t); code != 0 {
		t.Fatal(code, command.diagnostic.String())
	}
	f.git.Git(t, command.app.directory, "push", "-o", "oberth.run=1", "oberth", "HEAD:refs/heads/profile-space")
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if run.Origin.Actor != "checkout-agent" {
		t.Fatal("push lost the joined identity", run.Origin.Actor)
	}
}
