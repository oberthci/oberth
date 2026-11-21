package contracttest

import (
	"context"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type NativeProgress interface {
	Run(context.Context, string) (stateport.Run, error)
	Nodes(context.Context, string) ([]stateport.Node, error)
	AdvanceNative(context.Context, string) error
	SaveSteps(context.Context, string, []stepmodel.RunStep) error
	BeginEvidence(context.Context, stateport.Action, stateport.EvidenceStart) (int64, error)
	PutNodes(context.Context, stateport.Action, stateport.NodeBatch) error
	CompleteEvidence(context.Context, stateport.Action, stateport.EvidenceCompletion) error
}

// ProgressFixture supplies an admitted run and reads independently persisted facts.
// Revision is the store's run-control revision, not its observation sequence.
type ProgressFixture struct {
	Store    NativeProgress
	RunID    string
	Path     string
	Revision func(context.Context) (int64, error)
	Steps    func(context.Context) ([]stepmodel.RunStep, error)
}

func Progress(t *testing.T, factory func(*testing.T) ProgressFixture) {
	t.Helper()
	f := factory(t)
	ctx := t.Context()
	revision, err := f.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection(t, f)
	seq, err := f.Store.BeginEvidence(ctx, stateport.Action{}, stateport.EvidenceStart{RunID: f.RunID, Nodes: 1})
	if err != nil {
		t.Fatal(err)
	}
	batch := stateport.NodeBatch{RunID: f.RunID, Sequence: seq, Nodes: []stateport.Node{{NodeID: "node", Path: f.Path, Status: stepmodel.StepFailed, Attempt: 1, Credential: new(false)}}}
	wrong := batch
	wrong.Offset = 1
	if err = f.Store.PutNodes(ctx, stateport.Action{}, wrong); err == nil {
		t.Fatal("out-of-order evidence accepted")
	}
	if err = f.Store.CompleteEvidence(ctx, stateport.Action{}, stateport.EvidenceCompletion{RunID: f.RunID, Sequence: seq}); err == nil {
		t.Fatal("missing batch completed")
	}
	if err = f.Store.PutNodes(ctx, stateport.Action{}, batch); err != nil {
		t.Fatal(err)
	}
	if err = f.Store.PutNodes(ctx, stateport.Action{}, batch); err == nil {
		t.Fatal("duplicate node accepted")
	}
	retainedNativeEvidence(t, f, batch)
	projection(t, f)
	nativeRestartRetention(t, f, batch.Nodes[0])
	nativeOrderingIdentity(t, f, batch.Nodes[0])
	nativeStepRefusals(t, f)
	if got, err := f.Revision(ctx); err != nil || got != revision {
		t.Fatal("progress changed control revision", got, err)
	}
}

func projection(t *testing.T, f ProgressFixture) {
	t.Helper()
	ctx := t.Context()
	var err error
	steps := []stepmodel.RunStep{{Path: f.Path, Name: "build", Status: stepmodel.StepFailed, DisplayReason: "out of memory"}}
	if err = f.Store.SaveSteps(ctx, f.RunID, steps); err != nil {
		t.Fatal(err)
	}
	steps[0].DisplayReason = "changed"
	saved, err := f.Steps(ctx)
	if err != nil || len(saved) != 1 || saved[0].DisplayReason != "out of memory" {
		t.Fatal("projection not retained", saved, err)
	}
}

func retainedNativeEvidence(t *testing.T, f ProgressFixture, batch stateport.NodeBatch) {
	t.Helper()
	ctx := t.Context()
	nodes, err := f.Store.Nodes(ctx, f.RunID)
	if err != nil || !reflect.DeepEqual(nodes, batch.Nodes) {
		t.Fatal("native nodes differ", nodes, err)
	}
	if err = f.Store.CompleteEvidence(ctx, stateport.Action{}, stateport.EvidenceCompletion{RunID: f.RunID, Sequence: batch.Sequence}); err != nil {
		t.Fatal(err)
	}
	complete, err := f.Store.Run(ctx, f.RunID)
	if err != nil || !complete.EvidenceComplete {
		t.Fatal("completion not retained", complete, err)
	}
	if err = f.Store.AdvanceNative(ctx, f.RunID); err != nil {
		t.Fatal(err)
	}
	incomplete, err := f.Store.Run(ctx, f.RunID)
	if err != nil || incomplete.EvidenceComplete {
		t.Fatal("incomplete evidence was not marked", incomplete, err)
	}
}

func nativeStepRefusals(t *testing.T, f ProgressFixture) {
	t.Helper()
	badSteps := []stepmodel.RunStep{{Path: "unadmitted", Status: stepmodel.StepRunning}}
	if err := f.Store.SaveSteps(t.Context(), f.RunID, badSteps); err == nil {
		t.Fatal("unadmitted projection accepted")
	}
	badSteps[0].Path, badSteps[0].Status = f.Path, "invalid"
	if err := f.Store.SaveSteps(t.Context(), f.RunID, badSteps); err == nil {
		t.Fatal("invalid projection accepted")
	}
	good := stepmodel.RunStep{Path: f.Path, Status: stepmodel.StepRunning}
	bad := [][]stepmodel.RunStep{{good, good}, {{Path: f.Path, Status: stepmodel.StepRunning, AttemptCount: 1}}, {{Path: f.Path, Status: stepmodel.StepRunning, Attempts: []stepmodel.StepAttempt{{NodeID: "n", Status: stepmodel.StepRunning}}}}}
	if f.Path == "build" {
		bad = append(bad, []stepmodel.RunStep{{Path: "build(0:a)", Status: stepmodel.StepRunning}})
	}
	for _, steps := range bad {
		if err := f.Store.SaveSteps(t.Context(), f.RunID, steps); err == nil {
			t.Fatal("invalid native step projection accepted", steps)
		}
	}
	good.AttemptCount = 1
	good.Attempts = []stepmodel.StepAttempt{{NodeID: "n", Status: stepmodel.StepRunning}}
	mustRun(t, f.Store.SaveSteps(t.Context(), f.RunID, []stepmodel.RunStep{good}))
}

func nativeOrderingIdentity(t *testing.T, f ProgressFixture, node stateport.Node) {
	t.Helper()
	node.PodRestarted = true
	node.Order = []int{1, 0}
	seq, err := f.Store.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: f.RunID, Nodes: 1})
	mustRun(t, err)
	mustRun(t, f.Store.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: f.RunID, Sequence: seq, Nodes: []stateport.Node{node}}))
	node.Order[0] = 9
	got, err := f.Store.Nodes(t.Context(), f.RunID)
	mustRun(t, err)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Order, []int{1, 0}) {
		t.Fatal("native retry order not independently retained", got)
	}
	got[0].Order[0] = 7
	got, err = f.Store.Nodes(t.Context(), f.RunID)
	mustRun(t, err)
	if got[0].Order[0] != 1 {
		t.Fatal("native retry order escaped through read", got)
	}
	seq, err = f.Store.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: f.RunID, Nodes: 1})
	mustRun(t, err)
	if err = f.Store.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: f.RunID, Sequence: seq, Nodes: []stateport.Node{node}}); err == nil {
		t.Fatal("saved native retry order changed")
	}
}

func nativeRestartRetention(t *testing.T, f ProgressFixture, node stateport.Node) {
	t.Helper()
	node.PodRestarted = true
	seq, err := f.Store.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: f.RunID, Nodes: 1})
	mustRun(t, err)
	mustRun(t, f.Store.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: f.RunID, Sequence: seq, Nodes: []stateport.Node{node}}))
	nodes, err := f.Store.Nodes(t.Context(), f.RunID)
	mustRun(t, err)
	if len(nodes) != 1 || !nodes[0].PodRestarted {
		t.Fatal("native Pod restart was not retained", nodes)
	}
	node.PodRestarted = false
	seq, err = f.Store.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: f.RunID, Nodes: 1})
	mustRun(t, err)
	if err = f.Store.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: f.RunID, Sequence: seq, Nodes: []stateport.Node{node}}); err == nil {
		t.Fatal("native Pod restart was erased")
	}
}
