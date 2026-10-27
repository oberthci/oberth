package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type ScheduleOutcome struct {
	Entry   string
	Outcome string
	FiredAt time.Time
}

func (s *Store) RecordScheduleFire(ctx context.Context, repo, entry string, at time.Time, outcome string) error {
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(outcome) == "" {
		return fmt.Errorf("%w: repository and outcome are required", ErrInvalid)
	}
	return recordScheduleFire(ctx, s.db, repo, entry, at, outcome)
}

func recordScheduleFire(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, repo, entry string, at time.Time, outcome string) error {
	var cursor any
	if outcome == "fired" || outcome == "coalesced" {
		cursor = at.UTC().UnixNano()
	} else if entry != "" && (outcome == "skipped" || outcome == "failed" || outcome == "paused") {
		// Zero denotes a known-due entry with no surviving successful fire.
		// It preserves first-fire catch-up across a restart, without inventing
		// a successful timestamp or advancing the schedule to this attempt.
		cursor = int64(0)
	}
	_, err := q.ExecContext(ctx, `
INSERT INTO schedule_fires(repo, entry, fired_at, outcome, last_fired_at) VALUES(?, ?, ?, ?, ?)
ON CONFLICT(repo, entry) DO UPDATE SET
    fired_at = excluded.fired_at, outcome = excluded.outcome,
    last_fired_at = CASE WHEN excluded.outcome IN ('fired', 'coalesced') THEN excluded.last_fired_at
        ELSE COALESCE(schedule_fires.last_fired_at, excluded.last_fired_at) END`,
		repo, entry, at.UTC().UnixNano(), outcome, cursor)
	if err != nil {
		return fmt.Errorf("record schedule fire: %w", err)
	}
	return nil
}

func (s *Store) ScheduleFires(ctx context.Context, repo string) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT entry, last_fired_at FROM schedule_fires WHERE repo = ? AND last_fired_at IS NOT NULL`, repo)
	if err != nil {
		return nil, fmt.Errorf("read schedule fires: %w", err)
	}
	defer func() { _ = rows.Close() }()
	fires := map[string]time.Time{}
	for rows.Next() {
		var entry string
		var firedAt int64
		if err := rows.Scan(&entry, &firedAt); err != nil {
			return nil, fmt.Errorf("scan schedule fire: %w", err)
		}
		if firedAt == 0 {
			fires[entry] = time.Time{}
		} else {
			fires[entry] = time.Unix(0, firedAt).UTC()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schedule fires: %w", err)
	}
	return fires, nil
}

func (s *Store) ScheduleOutcomes(ctx context.Context, repo string) ([]ScheduleOutcome, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT entry, outcome, fired_at FROM schedule_fires WHERE repo = ? ORDER BY entry`, repo)
	if err != nil {
		return nil, fmt.Errorf("read schedule outcomes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var outcomes []ScheduleOutcome
	for rows.Next() {
		var outcome ScheduleOutcome
		var firedAt int64
		if err := rows.Scan(&outcome.Entry, &outcome.Outcome, &firedAt); err != nil {
			return nil, fmt.Errorf("scan schedule outcome: %w", err)
		}
		outcome.FiredAt = time.Unix(0, firedAt).UTC()
		outcomes = append(outcomes, outcome)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schedule outcomes: %w", err)
	}
	return outcomes, nil
}
