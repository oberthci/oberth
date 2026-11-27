package store

import (
	"context"
	"database/sql"

	"github.com/oberthci/oberth/internal/stateport"
)

// BeginEvidence fences observations with their sequence, independently of the run revision.
func (s *Store) BeginEvidence(ctx context.Context, action stateport.Action, input stateport.EvidenceStart) (int64, error) {
	if input.Nodes < 1 {
		return 0, refuse(invalid, nil)
	}
	var sequence int64
	err := s.observe(ctx, func(tx *sql.Tx) error {
		if err := requireUnhandedRun(ctx, tx, input.RunID); err != nil {
			return err
		}
		if err := s.runChange(ctx, tx, input.RunID); err != nil {
			return err
		}
		if _, err := readPlan(ctx, tx, input.RunID); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `UPDATE runs SET evidence_sequence=evidence_sequence+1,
   evidence_expected=?,evidence_received=0,evidence_complete=0
   WHERE id=? RETURNING evidence_sequence`, input.Nodes, input.RunID).Scan(&sequence)
		return databaseError(err)
	}, action.Events...)
	return sequence, err
}

func advanceEvidence(ctx context.Context, tx *sql.Tx, batch stateport.NodeBatch) error {
	if err := requireUnhandedRun(ctx, tx, batch.RunID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET evidence_received=evidence_received+?,
  evidence_complete=0 WHERE id=? AND evidence_sequence=?
  AND evidence_sequence>0 AND evidence_received=? AND evidence_received+?<=evidence_expected`,
		len(batch.Nodes), batch.RunID, batch.Sequence, batch.Offset, len(batch.Nodes))
	if err != nil {
		return databaseError(err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return refuse(staleRevision, err)
	}
	return nil
}

func requireEvidenceSequence(ctx context.Context, tx *sql.Tx, input stateport.EvidenceCompletion) error {
	var ready bool
	err := tx.QueryRowContext(ctx, `SELECT evidence_sequence=? AND evidence_sequence>0
  AND evidence_received=evidence_expected FROM runs WHERE id=?`, input.Sequence, input.RunID).Scan(&ready)
	if err != nil {
		return databaseError(err)
	}
	if !ready {
		return refuse(incompleteEvidence, nil)
	}
	return nil
}
