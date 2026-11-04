package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// Every execution of a parked promotion retains its reason, including recovery.
func (s *Store) recordPromotionBranchCISkippedTx(ctx context.Context, tx *sql.Tx, promotion model.Promotion, run model.Run) (model.Run, error) {
	if !promotion.BranchCISkipped {
		return run, nil
	}
	if run.Reason != "" {
		run.Reason += "; "
	}
	run.Reason += "promoted without branch CI"
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET reason=? WHERE id=?`, run.Reason, run.ID); err != nil {
		return model.Run{}, fmt.Errorf("record skipped branch CI: %w", err)
	}
	return run, nil
}
