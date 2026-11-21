package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestEvidenceSequenceResetsAndRejectsStaleOffsets(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "run")
	a := stateport.Action{Actor: "operator"}
	node := completeNode(s)
	first, err := s.BeginEvidence(t.Context(), a, stateport.EvidenceStart{RunID: r.ID, Nodes: 2})
	requireOK(t, err)
	requireCode(t, s.PutNodes(t.Context(), a, stateport.NodeBatch{RunID: r.ID, Sequence: first, Offset: 1, Nodes: []stateport.Node{node}}), publicerr.Conflict)
	requireOK(t, s.PutNodes(t.Context(), a, stateport.NodeBatch{RunID: r.ID, Sequence: first, Nodes: []stateport.Node{node}}))
	requireCode(t, s.PutNodes(t.Context(), a, stateport.NodeBatch{RunID: r.ID, Sequence: first, Offset: 1, Nodes: []stateport.Node{node}}), publicerr.Conflict)
	second, err := s.BeginEvidence(t.Context(), a, stateport.EvidenceStart{RunID: r.ID, Nodes: 1})
	requireOK(t, err)
	stale := node
	stale.NodeID = "stale-new"
	requireCode(t, s.PutNodes(t.Context(), a, stateport.NodeBatch{RunID: r.ID, Sequence: first, Nodes: []stateport.Node{stale}}), publicerr.Conflict)
	requireOK(t, s.PutNodes(t.Context(), a, stateport.NodeBatch{RunID: r.ID, Sequence: second, Nodes: []stateport.Node{node}}))
	extra := node
	extra.NodeID = "extra"
	requireCode(t, s.PutNodes(t.Context(), a, stateport.NodeBatch{RunID: r.ID, Sequence: second, Offset: 1, Nodes: []stateport.Node{extra}}), publicerr.Conflict)
	requireOK(t, s.CompleteEvidence(t.Context(), a, stateport.EvidenceCompletion{RunID: r.ID, Sequence: second}))
	_, err = s.BeginEvidence(t.Context(), a, stateport.EvidenceStart{RunID: r.ID, Nodes: 1})
	requireOK(t, err)
	got, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if got.EvidenceComplete {
		t.Fatal("new sequence retained a complete receipt")
	}
}
