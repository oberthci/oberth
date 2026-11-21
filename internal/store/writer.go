package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	"modernc.org/sqlite"
)

// SQLite's busy handler does not observe context cancellation. Immediate lock
// attempts plus a cancellable wait keep both the configured timeout and deadline.
func (s *Store) beginWrite(ctx context.Context) (*sql.Tx, func(), error) {
	wait, cancel := context.WithTimeout(ctx, s.options.BusyTimeout)
	defer cancel()
	select {
	case s.writes <- struct{}{}:
	case <-wait.Done():
		return nil, nil, refuse(locked, wait.Err())
	}
	release := func() { <-s.writes }
	for {
		tx, err := s.writer.BeginTx(ctx, nil)
		if err == nil {
			return tx, func() { _ = tx.Rollback(); release() }, nil
		}
		if cause := ctx.Err(); cause != nil {
			release()
			return nil, nil, refuse(locked, errors.Join(cause, err))
		}
		if !busy(err) {
			release()
			return nil, nil, databaseError(err)
		}
		if err := waitForWriter(wait); err != nil {
			release()
			return nil, nil, refuse(locked, err)
		}
	}
}

func busy(err error) bool {
	var database *sqlite.Error
	if !errors.As(err, &database) {
		return false
	}
	code := database.Code() & 255
	return code == 5
}

func waitForWriter(ctx context.Context) error {
	// Ten milliseconds bounds lock polling without occupying an SQL connection.
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Store) observe(ctx context.Context, apply func(*sql.Tx) error, events ...stateport.EventWrite) error {
	if err := validateEvents(events); err != nil {
		return err
	}
	tx, done, err := s.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer done()
	if err := apply(tx); err != nil {
		var same unchanged
		if errors.As(err, &same) {
			return nil
		}
		return err
	}
	if err := writeEvents(ctx, tx, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return databaseError(err)
	}
	s.signalChange()
	return nil
}
