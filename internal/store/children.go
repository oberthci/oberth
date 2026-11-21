package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
)

// ChildProof returns the persisted parent/child proof after native parent deletion.
// Native execution uses this for orphan recovery.
func (s *Store) ChildProof(ctx context.Context, runID string) (nativebinding.ChildBinding, error) {
	var body string
	var proof nativebinding.ChildBinding
	err := s.readers.QueryRowContext(ctx, `SELECT value FROM child_proofs WHERE run_id=?`, runID).Scan(&body)
	if err != nil {
		return proof, databaseError(err)
	}
	if err := json.Unmarshal([]byte(body), &proof); err != nil {
		return proof, refuse(corrupt, err)
	}
	return proof, nil
}

func persistChild(ctx context.Context, tx *sql.Tx, input stateport.Acceptance, repoID int64) error {
	if input.Child == nil {
		return nil
	}
	proof := *input.Child
	v := input.Prepared.View()
	if err := proof.Validate(); err != nil {
		return err
	}
	if proof.Run.RepositoryID != repoID ||
		proof.Run.RunID != v.RunID ||
		proof.Run.Identity != v.Digest ||
		proof.Run.SpecSHA256 != v.SpecDigest ||
		proof.Run.Revision.Repository != v.Repository ||
		proof.Run.Revision.Commit != v.CommitSHA ||
		proof.Run.Ref != v.Ref ||
		proof.ScheduleEntry != v.ScheduleEntry {
		return refuse(conflict, nil)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO child_proofs VALUES(?,?)`, v.RunID, encode(proof))
	return databaseError(err)
}
