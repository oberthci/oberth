package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// headWaitMaxAge bounds how old a recorded head-wait origin may be and still
// be inherited. A run interrupted while waiting at the head and never retried
// leaves a row behind; a much later run of the same commit starts afresh.
const headWaitMaxAge = 24 * time.Hour

// runDurationSamples is how many recent passed runs the duration estimate
// reads; runDurationMinSamples is the fewest it trusts.
const (
	runDurationSamples    = 20
	runDurationMinSamples = 3
)

// AdmissionHeadWait returns when a run of this commit first waited at the
// head of the admission queue, if that is recorded and recent (#1085).
func (s *Store) AdmissionHeadWait(ctx context.Context, repoID int64, refKind model.RefKind, ref, sha string) (time.Time, bool, error) {
	var since int64
	err := s.db.QueryRowContext(ctx, `SELECT since FROM admission_head_waits WHERE repo_id=? AND ref_kind=? AND ref=? AND sha=?`,
		repoID, string(refKind), ref, strings.ToLower(sha)).Scan(&since)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	recorded := fromUnixNano(since)
	if s.now().Sub(recorded) > headWaitMaxAge {
		return time.Time{}, false, s.SweepAdmissionHeadWaits(ctx)
	}
	return recorded, true, nil
}

// SweepAdmissionHeadWaits deletes every origin older than 24 h: heads whose
// run ended without passing through the gate's abandon path (an interrupted
// promotion or tag) would otherwise leave rows behind.
func (s *Store) SweepAdmissionHeadWaits(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admission_head_waits WHERE since < ?`, unixNano(s.now().Add(-headWaitMaxAge)))
	return err
}

// RecordAdmissionHeadWait records the first time a run of this commit waited
// at the head; an existing origin is kept.
func (s *Store) RecordAdmissionHeadWait(ctx context.Context, repoID int64, refKind model.RefKind, ref, sha string, since time.Time) error {
	if repoID <= 0 || strings.TrimSpace(ref) == "" || !validOID(strings.ToLower(sha)) {
		return fmt.Errorf("%w: head wait key is incomplete", ErrInvalid)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO admission_head_waits(repo_id, ref_kind, ref, sha, since) VALUES(?,?,?,?,?)
ON CONFLICT(repo_id, ref_kind, ref, sha) DO NOTHING`, repoID, string(refKind), ref, strings.ToLower(sha), unixNano(since))
	return err
}

// ClearAdmissionHeadWait forgets the origin once its run was admitted or ended.
func (s *Store) ClearAdmissionHeadWait(ctx context.Context, repoID int64, refKind model.RefKind, ref, sha string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admission_head_waits WHERE repo_id=? AND ref_kind=? AND ref=? AND sha=?`,
		repoID, string(refKind), ref, strings.ToLower(sha))
	return err
}

// RunDurationEstimate is the median duration of the repository's most recent
// passed runs of the same class, the bounded-backfill predictor (#1085). The
// classes are release (tag runs), promotion, schedule and branch (every other
// branch run); a release is never estimated from branch CI. ok is false with
// fewer than three samples. The median is advisory: a run may take longer.
func (s *Store) RunDurationEstimate(ctx context.Context, repoID int64, class string) (time.Duration, bool, error) {
	switch class {
	case "release", "promotion", "schedule", "branch":
	default:
		return 0, false, fmt.Errorf("%w: run duration class %q", ErrInvalid, class)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT finished_at - started_at FROM runs
WHERE repo_id = ?1 AND status = 'passed' AND started_at IS NOT NULL AND finished_at IS NOT NULL
  AND finished_at >= started_at
  AND ((?2 = 'release' AND release = 1)
    OR (?2 IN ('promotion', 'schedule') AND release = 0 AND trigger = ?2)
    OR (?2 = 'branch' AND release = 0 AND trigger NOT IN ('promotion', 'schedule', 'tag')))
ORDER BY queue_sequence DESC LIMIT ?3`, repoID, class, runDurationSamples)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = rows.Close() }()
	var samples []int64
	for rows.Next() {
		var duration int64
		if err := rows.Scan(&duration); err != nil {
			return 0, false, err
		}
		samples = append(samples, duration)
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	if len(samples) < runDurationMinSamples {
		return 0, false, nil
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return time.Duration(samples[len(samples)/2]), true, nil
}
