package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/model"
)

// RecordResourceAttempts retains every observed node, including superseded
// retries. A later snapshot cannot delete an earlier node or erase its failure.
func (s *Store) RecordResourceAttempts(ctx context.Context, i model.ResourceAttemptInventory) error {
	if i.Validate() != nil {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var repoID int64
	var workflow string
	if err := tx.QueryRowContext(ctx, `SELECT repo_id,job_name FROM runs WHERE id=?`, i.RunID).Scan(&repoID, &workflow); err != nil {
		return err
	}
	if workflow != i.WorkflowName {
		return ErrInvalid
	}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT observation_json FROM resource_attempt_inventories WHERE run_id=?`, i.RunID).Scan(&previous)
	if err == nil {
		var old model.ResourceAttemptInventory
		if json.Unmarshal([]byte(previous), &old) != nil || old.Validate() != nil || old.WorkflowName != i.WorkflowName || old.WorkflowUID != i.WorkflowUID || old.Namespace != i.Namespace || old.RunID != i.RunID {
			return ErrInvalid
		}
		// A stale active snapshot cannot establish completed observation coverage.
		if old.Terminal && !i.Terminal {
			i.Complete = old.Complete && i.Complete
		}
		i.Terminal = i.Terminal || old.Terminal
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM resource_attempts WHERE run_id=?`, i.RunID).Scan(&count); err != nil {
		return err
	}
	overflow := false
	for _, a := range i.Attempts {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT observation_json FROM resource_attempts WHERE run_id=? AND node_id=?`, i.RunID, a.NodeID).Scan(&raw)
		if err == nil {
			var old model.ResourceAttempt
			if json.Unmarshal([]byte(raw), &old) != nil {
				return ErrInvalid
			}
			if old.NodeID != a.NodeID || old.Stage != a.Stage || old.Step != a.Step || old.Template != a.Template || old.Owner != a.Owner || (old.PodUID != "" && a.PodUID != "" && old.PodUID != a.PodUID) {
				return ErrInvalid
			}
			if old.PodUID != "" {
				a.PodUID = old.PodUID
			}
			if old.Status == model.StepFailed || old.Status == model.StepTimedOut || old.Status.Progress() > a.Status.Progress() {
				a.Status = old.Status
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else if count >= 4096 {
			overflow = true
			continue
		} else {
			count++
		}
		body, err := json.Marshal(a)
		if err != nil || len(body) > 2048 {
			return ErrInvalid
		}
		if string(body) != raw {
			if _, err := tx.ExecContext(ctx, `INSERT INTO resource_attempts(run_id,node_id,observation_json) VALUES(?,?,?) ON CONFLICT(run_id,node_id) DO UPDATE SET observation_json=excluded.observation_json`, i.RunID, a.NodeID, string(body)); err != nil {
				return err
			}
		}
	}
	// Persist the full-cap fence even when the incoming batch exceeded it.
	// Rolling back here would silently lose the newly discovered attempts.
	i.Complete = i.Complete && !overflow && count < 4096
	body, err := json.Marshal(i)
	if err != nil || len(body) > 1024 {
		return ErrInvalid
	}
	if string(body) != previous {
		if _, err := tx.ExecContext(ctx, `INSERT INTO resource_attempt_inventories(run_id,repo_id,observation_json) VALUES(?,?,?) ON CONFLICT(run_id) DO UPDATE SET observation_json=excluded.observation_json`, i.RunID, repoID, string(body)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
