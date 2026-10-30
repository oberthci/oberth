package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/oberthci/oberth/internal/model"
)

const queuePriorityMigrationVersion = 34
const queuePrioritySchema = `
CREATE TABLE IF NOT EXISTS queue_priority_patterns (
 pattern TEXT PRIMARY KEY CHECK(length(pattern) BETWEEN 1 AND 256)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS run_priority_overrides (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 priority TEXT NOT NULL CHECK(priority IN ('high','highest')),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 512),
 actor TEXT NOT NULL,
 updated_at INTEGER NOT NULL
) WITHOUT ROWID;`

// These correlated expressions also work in RETURNING and in run-list CTEs.
// Priority tables do not shadow id/ref; run-list CTEs alias their rows as runs.
// Apply/plan cancellation guards protect external state, not queue urgency: those
// triggers remain ordinary work unless an admin pattern/bump or aging raises them.
// Only the server's tag-ref classification can get automatic release urgency.
// A branch cannot gain it through its name, release flag or trigger spelling.
// Only the oldest waiting tag per repository gets the automatic boost. Claimed
// runs still waiting for weight count too; an explicit revoke disables the boost.
// Size eligibility is checked after exact checkout, at the weighted gate.
const priorityTagReleaseSQL = `(ref_kind='tag' AND (release=1 OR trigger IN ('tag','release')))`
const tagPriorityEnabledSQL = `NOT EXISTS (SELECT 1 FROM size_grants AS priority_grant WHERE priority_grant.repo_id=runs.repo_id AND priority_grant.revoked_at IS NOT NULL)`

// v30 fills every durable admission_sequence. Use its existing status/order
// index to search waiting work, rather than walking a repository's log history.
const oldestWaitingTagSQL = `NOT EXISTS (
 SELECT 1 FROM runs AS earlier_tag INDEXED BY runs_admission_idx
 WHERE earlier_tag.status IN ('queued','running') AND earlier_tag.started_at IS NULL
 AND earlier_tag.repo_id=runs.repo_id AND earlier_tag.ref_kind='tag'
 AND (earlier_tag.release=1 OR earlier_tag.trigger IN ('tag','release'))
 AND (earlier_tag.admission_sequence,earlier_tag.queue_sequence) <
     (COALESCE(runs.admission_sequence,runs.queue_sequence),runs.queue_sequence)
)`
const automaticTagPrioritySQL = `(` + priorityTagReleaseSQL + ` AND ` + tagPriorityEnabledSQL + ` AND ` + oldestWaitingTagSQL + `)`
const naturalHighestSQL = `(trigger='promotion' OR ` + automaticTagPrioritySQL + `)`
const overridePrioritySQL = `(SELECT priority FROM run_priority_overrides WHERE run_id=id)`
const rcPatternSQL = `(SELECT pattern FROM queue_priority_patterns WHERE ref_kind='branch' AND ref GLOB pattern ORDER BY pattern LIMIT 1)`
const basePrioritySQL = `CASE WHEN ` + naturalHighestSQL + ` THEN 'highest' WHEN ` + overridePrioritySQL + ` IS NOT NULL THEN ` + overridePrioritySQL + ` WHEN ` + rcPatternSQL + ` IS NOT NULL THEN 'high' ELSE 'normal' END`
const priorityReasonSQL = `CASE WHEN trigger='promotion' THEN 'promotion run' WHEN ` + automaticTagPrioritySQL + ` THEN '` + model.TagReleasePriorityReason + `' WHEN ` + overridePrioritySQL + ` IS NOT NULL THEN 'admin bump: ' || (SELECT reason FROM run_priority_overrides WHERE run_id=id) WHEN ` + rcPatternSQL + ` IS NOT NULL THEN 'release-candidate pattern: ' || ` + rcPatternSQL + ` WHEN ` + priorityTagReleaseSQL + ` THEN CASE WHEN NOT ` + tagPriorityEnabledSQL + ` THEN 'tag/release automatic priority disabled by size_revoke' ELSE 'tag/release waiting behind an earlier tag of this repository' END ELSE 'ordinary run' END`

// priorityOrderSQL shares the effective class with the in-memory gate. The
// clock is rendered from an int64, never request text, and is fixed per query.
func priorityOrderSQL(now time.Time) string {
	base := basePrioritySQL
	return `CASE WHEN (` + base + `)='highest' OR ((` + base + `) IN ('normal','high') AND queued_at <= ` + fmt.Sprint(unixNano(now)) + ` - 3600000000000) THEN 2 WHEN (` + base + `)='high' THEN 1 ELSE 0 END DESC, admission_sequence, queue_sequence`
}

func (s *Store) QueuePriorityPatterns(ctx context.Context) ([]string, error) {
	return queuePriorityPatterns(ctx, s.reader())
}

func queuePriorityPatterns(ctx context.Context, reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]string, error) {
	rows, err := reader.QueryContext(ctx, `SELECT pattern FROM queue_priority_patterns ORDER BY pattern`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	patterns := []string{}
	for rows.Next() {
		var pattern string
		if err := rows.Scan(&pattern); err != nil {
			return nil, err
		}
		patterns = append(patterns, pattern)
	}
	return patterns, rows.Err()
}

// SetQueuePriorityPatterns replaces installation-wide administrator patterns.
// SQLite glob syntax is intentionally limited to literal characters, * and ?;
// both wildcards can match slash. An empty list disables RC classification.
func (s *Store) SetQueuePriorityPatterns(ctx context.Context, actor string, patterns []string) ([]string, error) {
	if strings.TrimSpace(actor) == "" || len(patterns) > 16 {
		return nil, fmt.Errorf("%w: actor required; at most 16 RC patterns", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, p := range patterns {
		if p == "" || len(p) > 256 || strings.TrimSpace(p) != p || strings.ContainsAny(p, "[]\\") || strings.IndexFunc(p, unicode.IsControl) >= 0 || seen[p] {
			return nil, fmt.Errorf("%w: RC patterns must be unique, 1..256 bytes, with only * and ? wildcards", ErrInvalid)
		}
		seen[p] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	previous, err := queuePriorityPatterns(ctx, tx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM queue_priority_patterns`); err != nil {
		return nil, err
	}
	for _, p := range patterns {
		if _, err = tx.ExecContext(ctx, `INSERT INTO queue_priority_patterns(pattern) VALUES(?)`, p); err != nil {
			return nil, err
		}
	}
	details, _ := json.Marshal(map[string]any{"rc_patterns": patterns, "previous_rc_patterns": previous, "starvation_minutes": 60})
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "queue.priority.config", ResourceType: "queue", ResourceID: "priority", Details: string(details)}, unixNano(s.now())); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.QueuePriorityPatterns(ctx)
}

// BumpRunPriority accepts only waiting work and only raises its effective class.
// The service owns admin authorization and holds its admission gate through
// this call; the durable state change and audit are atomic.
func (s *Store) BumpRunPriority(ctx context.Context, actor, id string, priority model.RunPriority, reason string) (model.Run, error) {
	reason = strings.TrimSpace(reason)
	if strings.TrimSpace(actor) == "" || id == "" || (priority != model.PriorityHigh && priority != model.PriorityHighest) || reason == "" || len(reason) > 512 || strings.IndexFunc(reason, unicode.IsControl) >= 0 {
		return model.Run{}, fmt.Errorf("%w: actor, run ID, high/highest priority and reason (1..512 bytes) required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Run{}, fmt.Errorf("%w: run", ErrNotFound)
	}
	if err != nil {
		return model.Run{}, err
	}
	current, _ := run.EffectivePriority(s.now())
	if !run.Status.Active() || run.StartedAt != nil || priority.Rank() <= current.Rank() {
		return model.Run{}, fmt.Errorf("%w: priority must raise a run awaiting admission", ErrInvalidState)
	}
	now := unixNano(s.now())
	if _, err = tx.ExecContext(ctx, `INSERT INTO run_priority_overrides(run_id,priority,reason,actor,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET priority=excluded.priority,reason=excluded.reason,actor=excluded.actor,updated_at=excluded.updated_at`, id, priority, reason, actor, now); err != nil {
		return model.Run{}, err
	}
	details, _ := json.Marshal(map[string]any{"previous_priority": current, "priority": priority, "reason": reason, "repo_id": run.RepoID})
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "run.priority", ResourceType: "run", ResourceID: id, Details: string(details)}, now); err != nil {
		return model.Run{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Run{}, err
	}
	return s.Run(ctx, id)
}

const claimableRunSQL = `status='queued' AND NOT EXISTS (SELECT 1 FROM publications AS publication WHERE publication.status='pending' AND publication.repo_id=runs.repo_id AND publication.ref_kind=runs.ref_kind AND publication.ref=runs.ref)`

func (s *Store) NextQueuedRun(ctx context.Context) (model.Run, error) {
	return scanRun(s.reader().QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE `+claimableRunSQL+` ORDER BY `+priorityOrderSQL(s.now())+` LIMIT 1`))
}

// inheritQueuePriorityTx preserves the same work item's wait and explicit bump
// across an automatic restart refire. Manual retries earn a new queue place.
func (s *Store) inheritQueuePriorityTx(ctx context.Context, tx *sql.Tx, previous, next model.Run) (model.Run, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET queued_at=? WHERE id=?`, unixNano(previous.QueuedAt), next.ID); err != nil {
		return model.Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_priority_overrides(run_id,priority,reason,actor,updated_at) SELECT ?,priority,reason,actor,updated_at FROM run_priority_overrides WHERE run_id=?`, next.ID, previous.ID); err != nil {
		return model.Run{}, err
	}
	return scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, next.ID))
}
