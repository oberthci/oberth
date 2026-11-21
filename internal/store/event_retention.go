package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

// RetainEvents prunes at most 512 rows per transaction. Each batch advances one
// watermark per repository and kind, so cursors detect internal and empty gaps.
// The composition root owns the retention calls; that consumer is not built yet.
func (s *Store) RetainEvents(ctx context.Context, ordinary, progress time.Time, events ...stateport.EventWrite) error {
	var cutoff int64
	err := s.readers.QueryRowContext(ctx, `SELECT id FROM events WHERE kind='run.updated' ORDER BY id DESC LIMIT 1 OFFSET 9999`).Scan(&cutoff)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return databaseError(err)
	}
	for _, selection := range []pruneSelection{
		{predicate: `kind='run.updated' AND id<?`, argument: cutoff},
		{predicate: `kind='run.updated' AND at<?`, argument: progress.UTC().Format(timestampFormat)},
		{predicate: `kind<>'run.updated' AND at<?`, argument: ordinary.UTC().Format(timestampFormat)},
	} {
		for {
			count, err := s.pruneEvents(ctx, selection, events)
			if err != nil {
				return err
			}
			if count < 512 {
				break
			}
			if err := maintenanceYield(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

type pruneSelection struct {
	predicate string
	argument  any
}

func (s *Store) pruneEvents(ctx context.Context, selection pruneSelection, events []stateport.EventWrite) (int64, error) {
	var count int64
	err := s.observe(ctx, func(tx *sql.Tx) error {
		ids := `SELECT id FROM events WHERE ` + selection.predicate + ` LIMIT 512`
		_, err := tx.ExecContext(ctx, `INSERT INTO event_pruned(repo,kind,through)
   SELECT repo,kind,max(id) FROM events WHERE id IN (`+ids+`) GROUP BY repo,kind
   ON CONFLICT(repo,kind) DO UPDATE SET through=max(through,excluded.through)`, selection.argument)
		if err != nil {
			return databaseError(err)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (`+ids+`)`, selection.argument)
		if err != nil {
			return databaseError(err)
		}
		count, err = result.RowsAffected()
		if err == nil && count == 0 {
			return unchanged{}
		}
		return databaseError(err)
	}, events...)
	return count, err
}

// Give independent handles two lock-poll opportunities before maintenance retries.
func maintenanceYield(ctx context.Context) error {
	if err := waitForWriter(ctx); err != nil {
		return err
	}
	return waitForWriter(ctx)
}

func eventGap(ctx context.Context, tx *sql.Tx, filter stateport.EventFilter) (bool, error) {
	// Zero starts a retained-history read; only a resume cursor can lose continuity.
	if filter.After == 0 {
		return false, nil
	}
	predicate, args := scopePredicate("repo", filter.ScopeKeys())
	var gap bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM event_pruned WHERE `+predicate+` AND through>?)`, append(args,
		filter.After)...).Scan(&gap)
	return gap, databaseError(err)
}
