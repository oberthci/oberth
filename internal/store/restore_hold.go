package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// ErrNoAdmissionHold marks a database whose schema predates the admission
// hold (before v0.17.85): a hold cannot be placed in it.
var ErrNoAdmissionHold = errors.New("store: database predates the admission hold")

// PlaceRestoreHold sets the administrator admission hold (no class allowed)
// in a restored database file before any server opens it, so a server older
// than the restored-runs marker (v0.21.0) does not dispatch the restored
// queued work again. It writes through the ordinary hold path, with its audit
// action, at the file's own schema: nothing is migrated.
//
// It places nothing and returns placed=false when no run is queued or
// running. A running run counts: a server older than the marker requeues one
// whose Workflow is gone. A hold the database already has is replaced and
// returned as previous: it may allow tag or release runs, or lapse minutes
// after the restore, and the hold placed here allows nothing and outlasts it
// (the audit action records the replaced hold).
func PlaceRestoreHold(ctx context.Context, path string, opts Options, actor, reason string, duration time.Duration) (placed bool, hold, previous model.AdmissionHold, resultErr error) {
	s, err := openCurrentUnverified(ctx, path, opts, false)
	if err != nil {
		return false, hold, previous, err
	}
	defer func() {
		// Fold the WAL into the main file so the caller ships one file.
		if resultErr == nil {
			if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				resultErr = fmt.Errorf("checkpoint the restored database: %w", err)
			}
		}
		resultErr = errors.Join(resultErr, s.db.Close())
	}()
	if err := s.verifyCurrentSchema(ctx); err != nil {
		return false, hold, previous, err
	}
	if err := s.verifyConfiguredProtocol(ctx); err != nil {
		return false, hold, previous, err
	}
	var tables int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='admission_holds'`).Scan(&tables); err != nil {
		return false, hold, previous, err
	}
	if tables == 0 {
		return false, hold, previous, ErrNoAdmissionHold
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE status IN ('queued','running')`).Scan(&active); err != nil {
		return false, hold, previous, err
	}
	if active == 0 {
		return false, hold, previous, nil
	}
	if previous, err = s.AdmissionHold(ctx); err != nil {
		return false, hold, previous, err
	}
	if !previous.On || !previous.ExpiresAt.After(s.now()) {
		previous = model.AdmissionHold{}
	}
	hold, err = s.SetAdmissionHold(ctx, actor, model.AdmissionHoldRequest{On: true, Reason: reason, Allow: []string{}, Duration: duration})
	return err == nil, hold, previous, err
}
