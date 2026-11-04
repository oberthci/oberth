package storetest

import (
	"context"
	"encoding/json"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// PutHistoricalResourceAttempts reconstructs retained records for reader/legacy
// fixtures. It deliberately bypasses current cross-record writer reconciliation
// and only operates on an explicitly registered isolated fixture database.
func PutHistoricalResourceAttempts(database *store.Store, ctx context.Context, i model.ResourceAttemptInventory) error {
	if i.Validate() != nil {
		return store.ErrInvalid
	}
	db, _, err := fixtureDB(database)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var repoID int64
	if err := tx.QueryRowContext(ctx, `SELECT repo_id FROM runs WHERE id=?`, i.RunID).Scan(&repoID); err != nil {
		return err
	}
	body, err := json.Marshal(i)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO resource_attempt_inventories(run_id,repo_id,observation_json) VALUES(?,?,?) ON CONFLICT(run_id) DO UPDATE SET observation_json=excluded.observation_json`, i.RunID, repoID, string(body)); err != nil {
		return err
	}
	for _, a := range i.Attempts {
		body, err := json.Marshal(a)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO resource_attempts(run_id,node_id,observation_json) VALUES(?,?,?) ON CONFLICT(run_id,node_id) DO UPDATE SET observation_json=excluded.observation_json`, i.RunID, a.NodeID, string(body)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
