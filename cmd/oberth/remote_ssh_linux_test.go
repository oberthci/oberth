//go:build linux

package main

import (
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestPerRemoteTransportPushesWithPersonalSSHOverrides(t *testing.T) {
	f, command, _, _ := joinedCheckoutFixture(t)
	ssh := recordSSH(t, command.app.directory)
	configureOtherSSH(t, &ssh, "environment-command", "ssh://git@forge-other.oberth.ci:2222/team/widget.git")
	command.app.git = ssh.git
	if code := command.run(t); code != 0 {
		t.Fatal(code, command.diagnostic.String())
	}
	if _, err := ssh.git(t.Context(), []string{"push", "-o", "oberth.run=1", "oberth", "HEAD:refs/heads/per-remote"}); err != nil {
		t.Fatal("Oberth push used the person's SSH command", err)
	}
	if _, err := os.Stat(ssh.record); !os.IsNotExist(err) {
		t.Fatal("Oberth push offered its identity through another SSH command", err)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if run.Origin.Actor != "checkout-agent" {
		t.Fatal("push lost the joined identity", run.Origin.Actor)
	}
}
