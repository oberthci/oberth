package store

import (
	"context"
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
	_, err := s.db.ExecContext(ctx, `
INSERT INTO schedule_fires(repo, entry, fired_at, outcome) VALUES(?, ?, ?, ?)
ON CONFLICT(repo, entry) DO UPDATE SET fired_at = excluded.fired_at, outcome = excluded.outcome`,
		repo, entry, at.UTC().UnixNano(), outcome)
	if err != nil {
		return fmt.Errorf("record schedule fire: %w", err)
	}
	return nil
}

func (s *Store) ScheduleFires(ctx context.Context, repo string) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT entry, fired_at FROM schedule_fires WHERE repo = ? AND outcome = 'fired'`, repo)
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
		fires[entry] = time.Unix(0, firedAt).UTC()
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
