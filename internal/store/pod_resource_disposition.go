package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/model"
)

// RecordPodResourceDisposition persists the executed Pod even if it never had a
// usable main runtime ID. Ownership is verified by the controller, then bound
// here to the run's current Workflow and immutable Pod/invocation identity.
func (s *Store) RecordPodResourceDisposition(ctx context.Context, f model.PodResourceDisposition) error {
	if f.Validate() != nil {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var repoID int64
	var workflow string
	if err := tx.QueryRowContext(ctx, `SELECT repo_id,job_name FROM runs WHERE id=?`, f.RunID).Scan(&repoID, &workflow); err != nil {
		return err
	}
	if workflow != f.WorkflowName {
		return ErrInvalid
	}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT observation_json FROM pod_resource_dispositions WHERE run_id=? AND node_id=? AND pod_uid=?`, f.RunID, f.NodeID, f.PodUID).Scan(&previous)
	if err == nil {
		var old model.PodResourceDisposition
		if json.Unmarshal([]byte(previous), &old) != nil || old.Validate() != nil {
			return ErrInvalid
		}
		check := f
		check.Reason, check.ObservedAt = old.Reason, old.ObservedAt
		if check != old {
			return ErrInvalid
		}
		// A late success or weaker unknown cause cannot erase a recorded failure.
		if old.Reason != "Completed" && (f.Reason == "Completed" || old.Reason != "Failed") {
			f.Reason = old.Reason
		}
		f.ObservedAt = old.ObservedAt
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	} else {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pod_resource_dispositions WHERE run_id=?`, f.RunID).Scan(&count); err != nil {
			return err
		}
		if count >= 2048 {
			return ErrInvalid
		}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(raw) > 3072 {
		return ErrInvalid
	}
	if string(raw) != previous {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pod_resource_dispositions(run_id,repo_id,node_id,pod_uid,burn,step,observation_json) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(run_id,node_id,pod_uid) DO UPDATE SET observation_json=excluded.observation_json`, f.RunID, repoID, f.NodeID, f.PodUID, f.Stage, f.Step, string(raw)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
