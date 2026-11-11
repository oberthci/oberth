package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// ObserveNodeBoot records a baseline only while the exact main container is
// running. A terminal first sighting cannot prove that its boot ID changed.
func (s *Store) ObserveNodeBoot(ctx context.Context, o model.NodeBootObservation, running bool) (*model.NodeReboot, error) {
	if !o.Valid() || (running && o.StartedAt.IsZero()) {
		return nil, fmt.Errorf("%w: node boot binding", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if running {
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO node_boot_observations
(run_id,workflow_name,workflow_uid,node_id,pod_uid,node_name,node_uid,boot_id,started_at,observed_at)
SELECT ?,?,?,?,?,?,?,?,?,? FROM runs WHERE id=? AND job_name=? AND status='running'
AND (SELECT count(*) FROM node_boot_observations WHERE run_id=?) < 2048`,
			o.RunID, o.WorkflowName, o.WorkflowUID, o.NodeID, o.PodUID, o.NodeName, o.NodeUID, o.BootID, unixNano(o.StartedAt), unixNano(o.ObservedAt), o.RunID, o.WorkflowName, o.RunID)
		if err != nil {
			return nil, err
		}
	}
	var name, nodeID, nodeName, nodeUID, bootID string
	var started, observed int64
	var changed sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT workflow_name,node_id,node_name,node_uid,boot_id,started_at,observed_at,changed_at FROM node_boot_observations WHERE run_id=? AND workflow_uid=? AND pod_uid=?`, o.RunID, o.WorkflowUID, o.PodUID).Scan(&name, &nodeID, &nodeName, &nodeUID, &bootID, &started, &observed, &changed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if name != o.WorkflowName || nodeID != o.NodeID || nodeName != o.NodeName || nodeUID != o.NodeUID || (!o.StartedAt.IsZero() && started != unixNano(o.StartedAt)) || unixNano(o.ObservedAt) < observed {
		return nil, nil
	}
	if bootID != o.BootID && !changed.Valid && unixNano(o.ObservedAt) > observed {
		changed = sql.NullInt64{Int64: unixNano(o.ObservedAt), Valid: true}
		if _, err = tx.ExecContext(ctx, `UPDATE node_boot_observations SET changed_at=? WHERE run_id=? AND workflow_uid=? AND pod_uid=? AND changed_at IS NULL`, changed.Int64, o.RunID, o.WorkflowUID, o.PodUID); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if !changed.Valid {
		return nil, nil
	}
	return &model.NodeReboot{NodeName: nodeName, After: fromUnixNano(observed), ObservedAt: fromUnixNano(changed.Int64)}, nil
}
