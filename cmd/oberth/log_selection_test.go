package main

import (
	"testing"

	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestDefaultLogSelectionDoesNotPickAnOldFailureOrChangeExplicitContainer(t *testing.T) {
	step := stepmodel.RunStep{Path: "build", Status: stepmodel.StepFailed,
		Attempts: []stepmodel.StepAttempt{{NodeID: "old", Status: stepmodel.StepFailed}, {NodeID: "new", Status: stepmodel.StepFailed}},
		Logs:     []stepmodel.LogSelector{{NodeID: "old", Container: "wait"}, {NodeID: "new", Container: "main"}},
	}
	selector := logcontract.Selector{Step: "build", StepSet: true}
	entries := []runlog.ContainerLog{{NodeID: "new", Container: "main", Main: true}}
	chosen, hints := defaultLogContainer([]stepmodel.RunStep{step}, selector, entries)
	if chosen.Container != "" || len(hints) != 0 {
		t.Fatal("an older executor failure replaced the current own container", chosen, hints)
	}
	selector.Container = "init"
	chosen, _ = defaultLogContainer([]stepmodel.RunStep{step}, selector, nil)
	if chosen != selector {
		t.Fatal("an explicit native selection was changed", chosen)
	}
	step.Status = stepmodel.StepPassed
	step.Logs = []stepmodel.LogSelector{{NodeID: "new", Container: "wait"}}
	selector.Container = ""
	chosen, _ = defaultLogContainer([]stepmodel.RunStep{step}, selector, nil)
	if chosen.Container != "" {
		t.Fatal("a successful step displayed executor output", chosen)
	}
}
