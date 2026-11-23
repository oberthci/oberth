//go:build linux

package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestComposedPushSelectsManualWorkflow(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	const file = ".oberth/walk.yaml"
	f.git.Write(t, file, []byte(strings.ReplaceAll(composedWorkflow, "echo", "printf")))
	commit := f.git.CommitFile(t, "add optional workflow")
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	f.git.Git(t, f.git.Work, "push", "oberth", "HEAD:refs/heads/walk", "-o", "oberth.run=1", "-o", "oberth.assist=0", "-o", "oberth.file="+file)
	run := f.waitMatchingRun(t, func(r stateport.Run) bool { return r.Ref == "refs/heads/walk" && r.Origin.WorkflowUID != "" })
	if run.Admission.SelectedFile != file || run.Admission.SourceReceipt.File != file || run.Admission.CommitSHA != commit || run.Release {
		t.Fatal("selected file was lost in receive", run.Admission.SelectedFile)
	}
	f.checkRetention(t, run)
	f.deliverSource(t, run)
	for _, path := range []string{".oberth/absent.yaml", ".oberth/release.yaml"} {
		output, err := f.pushResult(t, "refs/heads/refused", "-o", "oberth.run=1", "-o", "oberth.file="+path)
		if err == nil {
			t.Fatal("missing or release file accepted", path, output)
		}
		if path == ".oberth/release.yaml" && !strings.Contains(output, "release files require an admin tag") {
			t.Fatal("release refusal lost its reason", output)
		}
	}
}
