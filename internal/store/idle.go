package store

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// OpenIdleQueueReaders isolates the background queue sampler from the primary
// writer. The server calls this once at startup, before sharing the Store with
// goroutines. CLI handles use ordinary readers; inspection handles retain their
// private connection. Close owns this single-connection read-only WAL pool;
// it shares the live-reader policy
// used by SSH authentication readers.
func (s *Store) OpenIdleQueueReaders(ctx context.Context) error {
	if s.idleDB != nil {
		return nil
	}
	db, err := s.openLiveReaders(ctx, 1)
	if err != nil {
		return err
	}
	s.idleDB = db
	return nil
}

// IdleQueueStatus aggregates the entire live queue in one database snapshot;
// paginated/scoped run views cannot establish installation-wide quiescence.
func (s *Store) IdleQueueStatus(ctx context.Context) (model.IdleQueueStatus, error) {
	state := model.IdleQueueStatus{Version: 1}
	db := s.idleDB
	if db == nil {
		db = s.reader()
	}
	rows, err := db.QueryContext(ctx, `SELECT status, started_at IS NOT NULL, ref_kind, trigger, release, COUNT(*)
FROM runs WHERE status IN ('queued','running') GROUP BY status, started_at IS NOT NULL, ref_kind, trigger, release`)
	if err != nil {
		return state, fmt.Errorf("observe upgrade queue: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status, kind, trigger string
		var started, release bool
		var n int
		if err := rows.Scan(&status, &started, &kind, &trigger, &release, &n); err != nil {
			return state, err
		}
		if status == "running" && started {
			state.Running += n
		} else {
			state.Queued += n
		}
		switch {
		case release || kind == "tag" || trigger == "release" || trigger == "tag":
			state.Tag += n
		case trigger == "promotion":
			state.Promotion += n
		case trigger == "plan":
			state.Plan += n
		case trigger == "apply":
			state.Apply += n
		case kind == "branch" && (trigger == "" || trigger == "branch" || trigger == "push" || trigger == "schedule"):
			state.Branch += n
		default:
			state.Other += n
		}
	}
	return state, rows.Err()
}
