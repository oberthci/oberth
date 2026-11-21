package store

import (
	"context"
	"database/sql"
	"github.com/oberthci/oberth/internal/stateport"
)

// markArmedFailure reads native evidence inside the verdict's transaction.
func markArmedFailure(ctx context.Context, tx *sql.Tx, r *stateport.Run) error {
	if r.Origin.Assisted {
		return nil
	}
	for _, entry := range r.Admission.Plan.Entries {
		if !entry.HoldArmed {
			continue
		}
		nodes, err := readNodes(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		r.Origin.Assisted = stateport.ArmedStepMayHaveHeld(r.Admission.Plan, nodes)
		break
	}
	return nil
}
