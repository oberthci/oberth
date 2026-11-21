//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRealCompositionAssistRequestStaysUnarmed(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.freshDataDirectory(t)
	f.prepareCommit(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	output, err := f.pushResult(t, "refs/heads/assist-request", "-o", "oberth.run=1", "-o", "oberth.assist=1")
	if err != nil || strings.Count(output, "assist is not available in this version; running without it") != 1 {
		t.Fatal("disabled assist did not preserve one push notice", err, output)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if !run.Origin.AssistRequested || run.Origin.HasAssistance() {
		t.Fatal("assist request was lost or armed", run.Origin)
	}
	for _, entry := range run.Admission.Plan.Entries {
		if entry.HoldArmed {
			t.Fatal("forced-off preparation armed a hold", entry)
		}
	}
	holds, err := (heldRuns{f.owners.native}).List(t.Context(), run)
	if err != nil || len(holds) != 0 {
		t.Fatal("unarmed run lists a live hold", holds, err)
	}
	f.deliverSource(t, run)
	f.finishNative(t, run)
	final := f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunPassed })
	if !final.Origin.AssistRequested || final.Origin.HasAssistance() {
		t.Fatal("disabled assist disqualified ordinary completion", final.Origin)
	}
	f.checkOnce(t)
}
