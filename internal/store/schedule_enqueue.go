package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// EnqueueScheduleOccurrence consumes a due cursor and either enqueues its run
// and receive audit, or coalesces it into an active run of the same entry and
// branch. BEGIN IMMEDIATE serializes the cursor CAS and unbounded active lookup
// with all other queue mutations. Any read/write failure rolls everything back.
func (s *Store) EnqueueScheduleOccurrence(ctx context.Context, event model.ReceiveEventSpec, spec model.RunSpec, fire model.ScheduleFireSpec) (model.ScheduleEnqueueResult, error) {
	if err := validateReceiveEventSpec(event, true); err != nil {
		return model.ScheduleEnqueueResult{}, err
	}
	spec, err := validateRunSpec(spec)
	if err != nil {
		return model.ScheduleEnqueueResult{}, err
	}
	if err := matchReceiveRun(event, spec); err != nil {
		return model.ScheduleEnqueueResult{}, err
	}
	if spec.Trigger != "schedule" || spec.ScheduleEntry == "" || fire.At.IsZero() ||
		(!fire.HasPrevious && !fire.Previous.IsZero()) || !fire.At.After(fire.Previous) {
		return model.ScheduleEnqueueResult{}, fmt.Errorf("%w: cron fire requires a named schedule and a later cursor", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ScheduleEnqueueResult{}, fmt.Errorf("begin schedule enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRepositoryActive(ctx, tx, spec.RepoID); err != nil {
		return model.ScheduleEnqueueResult{}, err
	}
	qualified, err := qualifiedRepoName(ctx, tx, spec.RepoID)
	if err != nil {
		return model.ScheduleEnqueueResult{}, err
	}
	if qualified != fire.Repo {
		return model.ScheduleEnqueueResult{}, fmt.Errorf("%w: cron cursor repository does not match run", ErrInvalid)
	}
	var previous sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT last_fired_at FROM schedule_fires WHERE repo = ? AND entry = ?`, qualified, spec.ScheduleEntry).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return model.ScheduleEnqueueResult{}, fmt.Errorf("read schedule cursor: %w", err)
	}
	expected := int64(0)
	if !fire.Previous.IsZero() {
		expected = unixNano(fire.Previous)
	}
	if previous.Valid != fire.HasPrevious || (previous.Valid && previous.Int64 != expected) {
		return model.ScheduleEnqueueResult{Outcome: "stale"}, nil
	}

	active, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs
WHERE repo_id = ? AND ref_kind = 'branch' AND ref = ? AND trigger = 'schedule'
  AND schedule_entry = ? AND status IN ('queued', 'running')
ORDER BY queue_sequence LIMIT 1`, spec.RepoID, spec.Ref, spec.ScheduleEntry))
	result := model.ScheduleEnqueueResult{Outcome: "coalesced", Enqueued: model.EnqueueRunResult{Run: active, Duplicate: true}}
	if errors.Is(err, sql.ErrNoRows) {
		result.Outcome = "fired"
		result.Enqueued, err = s.enqueueReceiveEventTx(ctx, tx, event, spec)
	}
	if err != nil {
		return model.ScheduleEnqueueResult{}, fmt.Errorf("enqueue or coalesce schedule: %w", err)
	}
	if err := recordScheduleFire(ctx, tx, qualified, spec.ScheduleEntry, fire.At, result.Outcome); err != nil {
		return model.ScheduleEnqueueResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.ScheduleEnqueueResult{}, fmt.Errorf("commit schedule enqueue: %w", err)
	}
	return result, nil
}
