package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestNativeProgressNeverAppendsAuthorityAudit(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "run")
	a := stateport.Action{Actor: "observer"}
	before := independentHead(t, s.readers)
	sequence, err := s.BeginEvidence(t.Context(), a, stateport.EvidenceStart{RunID: r.ID, Nodes: 1})
	requireOK(t, err)
	node := completeNode(s)
	requireOK(t, s.PutNodes(t.Context(), a, stateport.NodeBatch{RunID: r.ID, Sequence: sequence, Nodes: []stateport.Node{node}}))
	requireOK(t, s.CompleteEvidence(t.Context(), a, stateport.EvidenceCompletion{RunID: r.ID, Sequence: sequence}))
	r, err = s.Run(t.Context(), r.ID)
	requireOK(t, err)
	requireOK(t, s.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed}))
	requireOK(t, s.ProjectCI(t.Context(), a, r.ID))
	if independentHead(t, s.readers) != before {
		t.Fatal("native observations grew the authority chain")
	}
}
