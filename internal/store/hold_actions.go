package store

import (
	"context"
	"database/sql"
	"github.com/oberthci/oberth/internal/stateport"
)

// Assist records the selected inspection intent before any native dispatch.
func (s *Store) Assist(ctx context.Context, a stateport.Action, q stateport.HoldAction) error {
	if !q.Valid() {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{a.Actor, "run.assist", "run", q.RunID, q.Audit(a.Reason)}, func(tx *sql.Tx) error {
		r, err := loadRun(ctx, tx, q.RunID)
		if err != nil {
			return err
		}
		if r.Revision != q.Revision {
			return refuse(staleRevision, nil)
		}
		if !r.Origin.AssistRequested {
			return refuse(assistNotRequested, nil)
		}
		if err = requirePlanPath(r.Admission.Plan, stateport.Node{Path: q.Step}); err != nil {
			return err
		}
		if q.Kind == "exec" {
			r.Origin.Assisted = true
		}
		_, err = tx.ExecContext(ctx, `UPDATE runs SET origin=?,revision=revision+1 WHERE id=?`, encode(r.Origin), r.ID)
		return databaseError(err)
	}, a.Events...)
}
