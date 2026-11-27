package store

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestEvidenceSequenceRejectsOverlappingBatchWithoutConsumingIt(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "run")
	a := stateport.Action{Actor: "observer"}
	sequence, err := s.BeginEvidence(t.Context(), a, stateport.EvidenceStart{RunID: r.ID, Nodes: 2})
	requireOK(t, err)
	first := completeNode(s)
	requireOK(t, s.PutNodes(t.Context(), a, stateport.NodeBatch{
		RunID: r.ID, Sequence: sequence, Nodes: []stateport.Node{first},
	}))
	second := first
	second.NodeID, second.Attempt = "second", 2
	batch := stateport.NodeBatch{RunID: r.ID, Sequence: sequence, Nodes: []stateport.Node{second}}
	changed := observeChanges(t, s)
	err = s.PutNodes(t.Context(), a, batch)
	want := "conflict: run revision changed - Read the run again and submit the current revision."
	if err == nil || err.Error() != want {
		t.Fatalf("overlapping batch: %v; want %s", err, want)
	}
	nodes, err := s.Nodes(t.Context(), r.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(nodes, []stateport.Node{first}) {
		t.Fatalf("overlapping batch changed nodes: %#v", nodes)
	}
	select {
	case <-changed:
		t.Fatal("overlapping batch notified a change")
	default:
	}
	batch.Offset = 1
	requireOK(t, s.PutNodes(t.Context(), a, batch))
	requireOK(t, s.CompleteEvidence(t.Context(), a, stateport.EvidenceCompletion{RunID: r.ID, Sequence: sequence}))
	nodes, err = s.Nodes(t.Context(), r.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(nodes, []stateport.Node{first, second}) {
		t.Fatalf("corrected batch did not retain both observations: %#v", nodes)
	}
	got, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if !got.EvidenceComplete {
		t.Fatal("corrected sequence did not complete")
	}
}
