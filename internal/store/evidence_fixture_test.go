package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

type evidenceInput struct {
	stateport.Action

	RunID string
	Nodes []stateport.Node
}

func putNodes(t *testing.T, s *Store, input evidenceInput) error {
	t.Helper()
	if len(input.Nodes) > 128 {
		return s.PutNodes(t.Context(), input.Action, stateport.NodeBatch{RunID: input.RunID, Nodes: input.Nodes})
	}
	sequence, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: input.RunID, Nodes: len(input.Nodes)})
	if err != nil {
		return err
	}
	return s.PutNodes(t.Context(), input.Action, stateport.NodeBatch{RunID: input.RunID, Sequence: sequence, Nodes: input.Nodes})
}

func completeEvidence(t *testing.T, s *Store, input evidenceInput) error {
	t.Helper()
	var sequence int64
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT evidence_sequence FROM runs WHERE id=?`, input.RunID).Scan(&sequence))
	return s.CompleteEvidence(t.Context(), input.Action, stateport.EvidenceCompletion{RunID: input.RunID, Sequence: sequence})
}
