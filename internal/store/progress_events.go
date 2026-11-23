package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oberthci/oberth/internal/stateport"
)

type progressSnapshot struct {
	run   stateport.Run
	steps string
}

func readProgress(ctx context.Context, tx *sql.Tx, id string) (progressSnapshot, error) {
	run, err := loadRun(ctx, tx, id)
	if err != nil {
		return progressSnapshot{}, err
	}
	before := progressSnapshot{run: run}
	err = tx.QueryRowContext(ctx, `SELECT value FROM native_steps WHERE run_id=?`, id).Scan(&before.steps)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return before, databaseError(err)
}

func (s *Store) progressEvent(ctx context.Context, tx *sql.Tx, before progressSnapshot) error {
	after, err := readProgress(ctx, tx, before.run.ID)
	if err != nil {
		return err
	}
	if before.steps == after.steps && encode(before.run.RunSummary) == encode(after.run.RunSummary) {
		return unchanged{}
	}
	// The existing event allocator survives pruning and supplies the projection revision.
	var sequence int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT seq FROM sqlite_sequence WHERE name='events'),0)+1`).Scan(&sequence); err != nil {
		return databaseError(err)
	}
	_, err = insertEvent(ctx, tx, stateport.ProgressEvent(after.run, sequence, s.options.Clock()))
	return err
}
