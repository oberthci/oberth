package argojob

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

type refusedFinishCompletion struct {
	*reviewR2Progress
	completions int
}

func (p *refusedFinishCompletion) CompleteEvidence(context.Context, stateport.Action, stateport.EvidenceCompletion) error {
	p.completions++
	return errors.New("completion unavailable")
}
func TestArmedTerminalDoesNotWaitForEvidenceCompletion(t *testing.T) {
	t.Parallel()
	reporter := &reviewR2Reporter{}
	e, tr, _, _ := reviewR2Executor(t, reporter)
	tr.plan.Entries[0].HoldArmed = true
	progress := &refusedFinishCompletion{reviewR2Progress: &reviewR2Progress{}}
	e.config.Progress = progress
	final := Observation{Status: stepmodel.RunPassed, Complete: true, Nodes: []stateport.Node{{NodeID: "final", Path: "build", Status: stepmodel.StepPassed}}}
	if err := e.publish(t.Context(), tr, final); err != nil || !tr.completed || reporter.calls != 1 || progress.completions != 0 {
		t.Fatal("terminal acknowledgement depends on evidence completion", err, reporter.calls, progress.completions)
	}
}
