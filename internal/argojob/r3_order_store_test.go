package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	core "k8s.io/api/core/v1"
)

func TestNativeRetryOrderSurvivesStoreRoundTripAndPartialSnapshot(t *testing.T) {
	t.Parallel()
	r := requestForProgress()
	p := compositionAdmission(t, "retry-order", r)
	s := compositionStore(t)
	registerCompositionRepository(t, s, r.Repo)
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: p}}})
	compositionOK(t, err)
	native := &nativefake.Workflows{}
	b, err := argojob.New(argojob.Config{Runs: s, Workflows: native}).Create(t.Context(), p)
	compositionOK(t, err)
	w := native.Object
	w.Status.Phase = wf.WorkflowSucceeded
	first, last := nativeNode("first", ".build(0)(0)", wf.NodeFailed, 1), nativeNode("last", ".build(1)(0)", wf.NodeSucceeded, 2)
	first.Name, last.Name = w.Name+".build(0)(0)", w.Name+".build(1)(0)"
	w.Status.Nodes = wf.Nodes{
		"outer": {ID: "outer", Name: w.Name + ".build", Type: wf.NodeTypeRetry, Children: []string{"early", "late"}},
		"early": {ID: "early", Name: w.Name + ".build(0)", Type: wf.NodeTypeRetry, Children: []string{"first"}},
		"late":  {ID: "late", Name: w.Name + ".build(1)", Type: wf.NodeTypeRetry, Children: []string{"last"}},
		last.ID: last,
	}
	before := project(t, argojob.Projection{Workflow: w, Plan: p.View().Plan, Pods: []*core.Pod{nativePod(w, last, 0)}})
	persistOrderObservation(t, s, b.RunID, before)
	steps, err := s.Steps(t.Context(), b.RunID)
	compositionOK(t, err)
	nodes, err := s.Nodes(t.Context(), b.RunID)
	compositionOK(t, err)
	if len(nodes) != 1 || len(nodes[0].Order) != 2 || nodes[0].Order[0] != 1 || nodes[0].Attempt != 1 {
		t.Fatal("nested native position lost", nodes)
	}
	delete(w.Status.Nodes, last.ID)
	w.Status.Nodes[first.ID] = first
	after := project(t, argojob.Projection{Workflow: w, Plan: p.View().Plan, Pods: []*core.Pod{nativePod(w, first, 7)}, Previous: steps, PreviousNodes: nodes})
	if len(after.Steps) != 1 || after.Steps[0].Status != stepmodel.StepPassed || len(after.Steps[0].Attempts) != 2 || after.Steps[0].Attempts[1].NodeID != last.ID {
		t.Fatal("newly seen older retry replaced the saved success", after)
	}
	persistOrderObservation(t, s, b.RunID, after)
	saved, err := s.Nodes(t.Context(), b.RunID)
	compositionOK(t, err)
	if len(saved) != 2 {
		t.Fatal("partial snapshot lost history", saved)
	}
}
func persistOrderObservation(t *testing.T, s *store.Store, id string, o argojob.Observation) {
	t.Helper()
	compositionOK(t, s.SaveSteps(t.Context(), id, o.Steps))
	seq, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: len(o.Nodes)})
	compositionOK(t, err)
	compositionOK(t, s.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: id, Sequence: seq, Nodes: o.Nodes}))
}
