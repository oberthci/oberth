package contracttest

import (
	"context"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

type NativeStore interface {
	Run(context.Context, string) (stateport.Run, error)
	BindWorkflow(context.Context, stateport.Action, nativebinding.RunBinding) error
	BeginEvidence(context.Context, stateport.Action, stateport.EvidenceStart) (int64, error)
	PutNodes(context.Context, stateport.Action, stateport.NodeBatch) error
	CompleteEvidence(context.Context, stateport.Action, stateport.EvidenceCompletion) error
}
type NativeFixture struct {
	Store   NativeStore
	Binding nativebinding.RunBinding
}

// NativeSuite checks the binding and sequence contract against the owner and fake.
func NativeSuite(t *testing.T, factory func(*testing.T) NativeFixture) {
	t.Helper()
	f := factory(t)
	ctx := t.Context()
	b := f.Binding
	before, err := f.Store.Run(ctx, b.RunID)
	if err != nil {
		t.Fatal(err)
	}
	checkNativeBinding(t, f)
	if _, err = f.Store.BeginEvidence(ctx, stateport.Action{}, stateport.EvidenceStart{RunID: b.RunID}); err == nil {
		t.Fatal("zero evidence accepted")
	}
	sequence, err := f.Store.BeginEvidence(ctx, stateport.Action{}, stateport.EvidenceStart{RunID: b.RunID, Nodes: 1})
	if err != nil {
		t.Fatal(err)
	}
	node := stateport.Node{NodeID: "native", Path: "build", Attempt: 1, Status: stepmodel.StepFailed, Credential: new(false)}
	batch := stateport.NodeBatch{RunID: b.RunID, Sequence: sequence, Nodes: []stateport.Node{node}}
	wrongBatch := batch
	wrongBatch.Nodes = []stateport.Node{node}
	wrongBatch.Nodes[0].Attempt = 0
	if err = f.Store.PutNodes(ctx, stateport.Action{}, wrongBatch); err == nil {
		t.Fatal("zero attempt accepted")
	}
	completion := stateport.EvidenceCompletion{RunID: b.RunID, Sequence: sequence}
	if err = f.Store.CompleteEvidence(ctx, stateport.Action{}, completion); err == nil {
		t.Fatal("unwritten evidence completed")
	}
	if err = f.Store.PutNodes(ctx, stateport.Action{}, batch); err != nil {
		t.Fatal(err)
	}
	if err = f.Store.PutNodes(ctx, stateport.Action{}, batch); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err = f.Store.CompleteEvidence(ctx, stateport.Action{}, completion); err != nil {
		t.Fatal(err)
	}
	after, err := f.Store.Run(ctx, b.RunID)
	if err != nil || after.Revision != before.Revision || !after.EvidenceComplete {
		t.Fatal("native facts changed control revision", after, err)
	}
}

func checkNativeBinding(t *testing.T, f NativeFixture) {
	t.Helper()
	ctx, b := t.Context(), f.Binding
	var err error
	if err = f.Store.BindWorkflow(ctx, stateport.Action{Actor: "operator"}, b); err != nil {
		t.Fatal(err)
	}
	wrong := b
	wrong.Identity = b.SpecSHA256
	if err = f.Store.BindWorkflow(ctx, stateport.Action{Actor: "operator"}, wrong); err == nil {
		t.Fatal("spec digest accepted as admission digest")
	}
}
