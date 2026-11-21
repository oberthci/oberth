package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oberthci/oberth/internal/stateport"
)

// Cancel compares the prepared run revision and atomically chooses a durable mode.
func (s *Store) Cancel(ctx context.Context, action stateport.Action, input stateport.RunCancellation) (stateport.Obligation, error) {
	var result stateport.Obligation
	details := map[string]any{"reason": action.Reason}
	m := mutation{action.Actor, "run.cancel", "run", input.ID, details}
	err := s.change(ctx, m, func(tx *sql.Tx) error {
		r, err := loadRun(ctx, tx, input.ID)
		if err != nil {
			return err
		}
		if r.Revision != input.Revision {
			return refuse(staleRevision, nil)
		}
		if terminal(r.Status) {
			return refuse(terminalRun, nil)
		}
		result, err = chooseCancellation(ctx, tx, action, input)
		if err != nil {
			return err
		}
		details["obligation"] = result
		_, err = tx.ExecContext(ctx, `UPDATE runs SET revision=revision+1 WHERE id=?`, input.ID)
		return databaseError(err)
	}, action.Events...)
	if err != nil {
		return stateport.Obligation{}, err
	}
	return result, nil
}

func chooseCancellation(ctx context.Context, tx *sql.Tx, action stateport.Action, input stateport.RunCancellation) (stateport.Obligation, error) {
	result := stateport.Obligation{RunID: input.ID, Kind: "stop", Value: stateport.ObligationStop, Actor: action.Actor}
	err := tx.QueryRowContext(ctx, `SELECT revision FROM obligations WHERE run_id=? AND kind='stop'`, input.ID).Scan(&result.Revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, databaseError(err)
	}
	if input.Terminate || result.Revision > 0 {
		result.Value = stateport.ObligationTerminate
	}
	result.Revision++
	_, err = tx.ExecContext(ctx, `INSERT INTO obligations VALUES(?,?,?,?,?) ON CONFLICT(run_id,kind)
DO UPDATE SET revision=excluded.revision,value=excluded.value,actor=excluded.actor`, result.RunID, result.Kind, result.Revision, result.Value, result.Actor)
	return result, databaseError(err)
}
