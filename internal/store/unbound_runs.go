package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oberthci/oberth/internal/stateport"
)

const unboundQuery = `SELECT id FROM runs INDEXED BY run_unbound WHERE status IN ('queued','running') AND admission IS NOT NULL AND
json_extract(origin,'$.workflow_uid')='' AND COALESCE(json_extract(origin,'$.resumed_from'),'')=''`

// UnboundRuns pages admitted, nonterminal runs awaiting a Workflow identity.
// Each admission is a detached full value, read in the same snapshot as its run.
func (s *Store) UnboundRuns(ctx context.Context, before int64, limit int) ([]stateport.Run, error) {
	if err := validateRunFilter(stateport.RunFilter{Before: before, Limit: limit}); err != nil {
		return nil, err
	}
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	query := unboundQuery
	var args []any
	if before != 0 {
		query += ` AND sequence<?`
		args = append(args, before)
	}
	query += ` ORDER BY sequence DESC LIMIT ?`
	args = append(args, limit)
	ids, err := unboundIDs(ctx, tx, query, args)
	if err != nil {
		return nil, err
	}
	result := make([]stateport.Run, 0, len(ids))
	for _, id := range ids {
		run, err := loadRun(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, nil
}

func unboundIDs(ctx context.Context, tx *sql.Tx, query string, args []any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, databaseError(err)
		}
		ids = append(ids, id)
	}
	return ids, databaseError(errors.Join(rows.Err(), rows.Close()))
}
