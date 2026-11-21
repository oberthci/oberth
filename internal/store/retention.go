package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oberthci/oberth/internal/stateport"
)

// BeginPurge records durable intent after checking current protection.
func (s *Store) BeginPurge(ctx context.Context, action stateport.Action, runID, receipt string) error {
	if receipt == "" {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{action.Actor, "logs.purge.begin", "run", runID, receipt}, func(tx *sql.Tx) error {
		if err := purgeable(ctx, tx, runID); err != nil {
			return err
		}
		var previous string
		err := tx.QueryRowContext(ctx, `SELECT receipt FROM purge WHERE run_id=?`, runID).Scan(&previous)
		if err == nil {
			if previous != receipt {
				return refuse(purgeReceiptChanged, nil)
			}
			return unchanged{}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return databaseError(err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO purge VALUES(?,?,0)`, runID, receipt)
		return databaseError(err)
	}, action.Events...)
}

// FinishPurge repeats protection checks before acknowledging removed evidence bytes.
func (s *Store) FinishPurge(ctx context.Context, action stateport.Action, runID, receipt string) error {
	return s.change(ctx, mutation{action.Actor, "logs.purge.finish", "run", runID, receipt}, func(tx *sql.Tx) error {
		if err := purgeable(ctx, tx, runID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE purge SET completed=1 WHERE run_id=? AND receipt=?`, runID, receipt)
		if err != nil {
			return databaseError(err)
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			return refuse(purgeReceiptChanged, err)
		}
		return nil
	}, action.Events...)
}

func purgeable(ctx context.Context, tx *sql.Tx, runID string) error {
	r, err := loadRun(ctx, tx, runID)
	if err != nil {
		return err
	}
	if !terminal(r.Status) {
		return refuse(purgeRunning, nil)
	}
	var protected bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM obligations WHERE run_id=? AND value<>'') OR EXISTS(SELECT 1 FROM publications
WHERE run_id=? AND status='pending')`, runID, runID).Scan(&protected)
	if err != nil {
		return databaseError(err)
	}
	if protected {
		return refuse(purgeProtected, nil)
	}
	return nil
}

// Readiness has a reserved connection and performs no mutation or audit scan.
func (s *Store) Readiness(ctx context.Context) error {
	return checkProtocol(ctx, s.readiness, stateport.Options{Protocol: s.options.Protocol, Schema: Schema})
}

// Idle reports durable obligations, not native execution capacity.
func (s *Store) Idle(ctx context.Context) (bool, error) {
	var busy bool
	err := s.idle.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE status IN ('queued','running')) OR EXISTS(SELECT 1 FROM obligations
WHERE value<>'') OR EXISTS(SELECT 1 FROM publications WHERE status='pending')`).Scan(&busy)
	return !busy, databaseError(err)
}

// PurgeReceipt returns the durable evidence-removal acknowledgement.
func (s *Store) PurgeReceipt(ctx context.Context, runID string) (stateport.PurgeReceipt, error) {
	result := stateport.PurgeReceipt{RunID: runID}
	err := s.readers.QueryRowContext(ctx, `SELECT receipt,completed FROM purge WHERE run_id=?`, runID).Scan(&result.ID, &result.Completed)
	return result, databaseError(err)
}
