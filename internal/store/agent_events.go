package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// Agent events, native lock history and promotion prune requests (#1514).
// Events and intervals are observations, not audited mutations: they never
// change a run, a promotion or a repository. Both tables are bounded by count.

const (
	maximumAgentEventKey      = 512
	maximumAgentEventPage     = 500
	maximumSyncIntervalPage   = 2000
	maximumPromotionPruneText = 1024
)

const agentEventColumns = `id, dedupe_key, kind, repo_id, repo, run_id, promotion_id, ref, sha, summary, data, created_at`

func scanAgentEvent(scan func(...any) error) (model.AgentEvent, error) {
	var event model.AgentEvent
	var data string
	var created int64
	if err := scan(&event.ID, &event.Key, &event.Kind, &event.RepoID, &event.Repo, &event.RunID, &event.PromotionID, &event.Ref, &event.SHA, &event.Summary, &data, &created); err != nil {
		return model.AgentEvent{}, err
	}
	event.At = fromUnixNano(created)
	if data != "" {
		if err := json.Unmarshal([]byte(data), &event.Data); err != nil {
			return model.AgentEvent{}, fmt.Errorf("decode agent event %d: %w", event.ID, err)
		}
	}
	return event, nil
}

// AppendAgentEvent records one event unless an event with the same key
// exists. It returns the stored event and whether this call inserted it.
// retain > 0 prunes the oldest events beyond that count.
func (s *Store) AppendAgentEvent(ctx context.Context, event model.AgentEvent, retain int) (model.AgentEvent, bool, error) {
	if !model.ValidAgentEventKind(event.Kind) || strings.TrimSpace(event.Key) == "" || len(event.Key) > maximumAgentEventKey {
		return model.AgentEvent{}, false, fmt.Errorf("%w: agent event kind and key are required", ErrInvalid)
	}
	summary := strings.Join(strings.Fields(event.Summary), " ")
	if runes := []rune(summary); len(runes) > model.MaximumAgentEventSummary {
		summary = string(runes[:model.MaximumAgentEventSummary-1]) + "…"
	}
	data := ""
	if len(event.Data) > 0 {
		encoded, err := json.Marshal(event.Data)
		if err != nil {
			return model.AgentEvent{}, false, fmt.Errorf("encode agent event: %w", err)
		}
		if len(encoded) > model.MaximumAgentEventData {
			return model.AgentEvent{}, false, fmt.Errorf("%w: agent event data exceeds %d bytes", ErrInvalid, model.MaximumAgentEventData)
		}
		data = string(encoded)
	}
	at := event.At
	if at.IsZero() {
		at = s.now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AgentEvent{}, false, fmt.Errorf("begin agent event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_events(dedupe_key, kind, repo_id, repo, run_id, promotion_id, ref, sha, summary, data, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, event.Key, event.Kind, event.RepoID, event.Repo, event.RunID, event.PromotionID, event.Ref, event.SHA, summary, data, unixNano(at))
	if err != nil {
		return model.AgentEvent{}, false, fmt.Errorf("record agent event: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return model.AgentEvent{}, false, err
	}
	stored, err := scanAgentEvent(tx.QueryRowContext(ctx, `SELECT `+agentEventColumns+` FROM agent_events WHERE dedupe_key = ?`, event.Key).Scan)
	if err != nil {
		return model.AgentEvent{}, false, fmt.Errorf("reload agent event: %w", err)
	}
	if inserted > 0 && retain > 0 && stored.ID%64 == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_events WHERE id <= ?`, stored.ID-int64(retain)); err != nil {
			return model.AgentEvent{}, false, fmt.Errorf("prune agent events: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return model.AgentEvent{}, false, fmt.Errorf("commit agent event: %w", err)
	}
	return stored, inserted > 0, nil
}

// AgentEvents lists events after filter.AfterID in ID order.
func (s *Store) AgentEvents(ctx context.Context, filter model.AgentEventFilter) ([]model.AgentEvent, error) {
	if filter.RepoIDs != nil && len(filter.RepoIDs) == 0 {
		return []model.AgentEvent{}, nil
	}
	limit := filter.Limit
	if limit <= 0 || limit > maximumAgentEventPage {
		limit = maximumAgentEventPage
	}
	query := `SELECT ` + agentEventColumns + ` FROM agent_events WHERE id > ?`
	args := []any{filter.AfterID}
	if filter.RepoIDs != nil {
		query += ` AND repo_id IN (` + placeholders(len(filter.RepoIDs)) + `)`
		for _, id := range filter.RepoIDs {
			args = append(args, id)
		}
	}
	if len(filter.Kinds) > 0 {
		query += ` AND kind IN (` + placeholders(len(filter.Kinds)) + `)`
		for _, kind := range filter.Kinds {
			args = append(args, kind)
		}
	}
	if filter.RunID != "" {
		query += ` AND run_id = ?`
		args = append(args, filter.RunID)
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, limit)
	rows, err := s.reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list agent events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	events := []model.AgentEvent{}
	for rows.Next() {
		event, err := scanAgentEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// Separate aggregates let SQLite seek the two rowid endpoints. Combining MIN
// and MAX in one aggregate scans the entire retained log for every subscriber
// wake/poll. Keep both seeks in one statement so they share a read snapshot.
const agentEventBoundsQuery = `SELECT (SELECT MIN(id) FROM agent_events), (SELECT MAX(id) FROM agent_events)`

// AgentEventBounds returns the oldest retained and the newest event IDs;
// both are zero when no event was ever retained.
func (s *Store) AgentEventBounds(ctx context.Context) (int64, int64, error) {
	var oldest, newest sql.NullInt64
	if err := s.reader().QueryRowContext(ctx, agentEventBoundsQuery).Scan(&oldest, &newest); err != nil {
		return 0, 0, fmt.Errorf("read agent event bounds: %w", err)
	}
	return oldest.Int64, newest.Int64, nil
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// RecordSyncObservation folds one sample of native lock participants into the
// interval history. observed lists the runs whose Workflow was read in this
// sample; active lists every run still active. A participant interval is
// extended when it is still present, opened when it is new, and closed when
// its run was observed without it or is no longer active. Runs that are
// active but could not be read keep their open intervals. retain > 0 prunes
// the oldest closed intervals beyond that count.
func (s *Store) RecordSyncObservation(ctx context.Context, at time.Time, participants []model.SyncParticipant, observed, active map[string]bool, retain int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sync observation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	open, err := openSyncIntervals(ctx, tx)
	if err != nil {
		return err
	}
	now := unixNano(at)
	present := map[string]bool{}
	for _, participant := range participants {
		if participant.Role != model.SyncHolder && participant.Role != model.SyncWaiter || participant.Lock == "" || participant.RunID == "" {
			continue
		}
		key := participant.Lock + "\x00" + participant.Role + "\x00" + participant.RunID
		if present[key] {
			continue
		}
		present[key] = true
		if existing, ok := open[key]; ok {
			if _, err := tx.ExecContext(ctx, `UPDATE sync_intervals SET last_seen = ? WHERE id = ?`, now, existing.id); err != nil {
				return fmt.Errorf("extend sync interval: %w", err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sync_intervals(lock_key, role, run_id, repo_id, repo, started_at, last_seen) VALUES(?,?,?,?,?,?,?)`,
			participant.Lock, participant.Role, participant.RunID, participant.RepoID, participant.Repo, now, now); err != nil {
			return fmt.Errorf("open sync interval: %w", err)
		}
	}
	for key, interval := range open {
		if present[key] || (active[interval.runID] && !observed[interval.runID]) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sync_intervals SET ended_at = ? WHERE id = ?`, now, interval.id); err != nil {
			return fmt.Errorf("close sync interval: %w", err)
		}
	}
	if retain > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sync_intervals WHERE ended_at IS NOT NULL AND id <= (SELECT MAX(id) FROM sync_intervals) - ?`, retain); err != nil {
			return fmt.Errorf("prune sync intervals: %w", err)
		}
	}
	return tx.Commit()
}

type openSyncInterval struct {
	id                int64
	lock, role, runID string
}

func openSyncIntervals(ctx context.Context, tx *sql.Tx) (map[string]openSyncInterval, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, lock_key, role, run_id FROM sync_intervals WHERE ended_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("read open sync intervals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	open := map[string]openSyncInterval{}
	for rows.Next() {
		var value openSyncInterval
		if err := rows.Scan(&value.id, &value.lock, &value.role, &value.runID); err != nil {
			return nil, err
		}
		open[value.lock+"\x00"+value.role+"\x00"+value.runID] = value
	}
	return open, rows.Err()
}

// SyncIntervals lists intervals newest first.
func (s *Store) SyncIntervals(ctx context.Context, filter model.SyncIntervalFilter) ([]model.SyncInterval, error) {
	if filter.RepoIDs != nil && len(filter.RepoIDs) == 0 {
		return []model.SyncInterval{}, nil
	}
	limit := filter.Limit
	if limit <= 0 || limit > maximumSyncIntervalPage {
		limit = maximumSyncIntervalPage
	}
	query := `SELECT id, lock_key, role, run_id, repo_id, repo, started_at, last_seen, ended_at FROM sync_intervals WHERE 1=1`
	var args []any
	if filter.Lock != "" {
		query += ` AND lock_key = ?`
		args = append(args, filter.Lock)
	}
	if !filter.Since.IsZero() {
		query += ` AND (ended_at IS NULL OR ended_at >= ?)`
		args = append(args, unixNano(filter.Since))
	}
	if filter.RepoIDs != nil {
		query += ` AND repo_id IN (` + placeholders(len(filter.RepoIDs)) + `)`
		for _, id := range filter.RepoIDs {
			args = append(args, id)
		}
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sync intervals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	intervals := []model.SyncInterval{}
	for rows.Next() {
		var value model.SyncInterval
		var started, seen int64
		var ended sql.NullInt64
		if err := rows.Scan(&value.ID, &value.Lock, &value.Role, &value.RunID, &value.RepoID, &value.Repo, &started, &seen, &ended); err != nil {
			return nil, err
		}
		value.StartedAt, value.LastSeen = fromUnixNano(started), fromUnixNano(seen)
		if ended.Valid {
			at := fromUnixNano(ended.Int64)
			value.EndedAt = &at
		}
		intervals = append(intervals, value)
	}
	return intervals, rows.Err()
}

// RequestPromotionPrune records a prune_source request for an admitted
// promotion. A second request for the same promotion is a no-op.
func (s *Store) RequestPromotionPrune(ctx context.Context, promotionID, actor string) error {
	if strings.TrimSpace(promotionID) == "" || strings.TrimSpace(actor) == "" {
		return fmt.Errorf("%w: promotion and actor are required", ErrInvalid)
	}
	now := unixNano(s.now())
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO promotion_prunes(promotion_id, actor, status, requested_at, updated_at) VALUES(?,?,'pending',?,?)`,
		promotionID, actor, now, now); err != nil {
		return fmt.Errorf("record promotion prune request: %w", err)
	}
	return nil
}

const promotionPruneColumns = `promotion_id, actor, status, reason, batch_id, requested_at, updated_at`

func scanPromotionPrune(scan func(...any) error) (model.PromotionPrune, error) {
	var value model.PromotionPrune
	var requested, updated int64
	if err := scan(&value.PromotionID, &value.Actor, &value.Status, &value.Reason, &value.BatchID, &requested, &updated); err != nil {
		return model.PromotionPrune{}, err
	}
	value.RequestedAt, value.UpdatedAt = fromUnixNano(requested), fromUnixNano(updated)
	return value, nil
}

// PromotionPrune returns the prune request of one promotion, or ErrNotFound.
func (s *Store) PromotionPrune(ctx context.Context, promotionID string) (model.PromotionPrune, error) {
	value, err := scanPromotionPrune(s.reader().QueryRowContext(ctx, `SELECT `+promotionPruneColumns+` FROM promotion_prunes WHERE promotion_id = ?`, promotionID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return model.PromotionPrune{}, fmt.Errorf("%w: promotion prune", ErrNotFound)
	}
	return value, err
}

// PendingPromotionPrunes lists the oldest pending requests.
func (s *Store) PendingPromotionPrunes(ctx context.Context, limit int) ([]model.PromotionPrune, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.reader().QueryContext(ctx, `SELECT `+promotionPruneColumns+` FROM promotion_prunes WHERE status = 'pending' ORDER BY requested_at LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending promotion prunes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var values []model.PromotionPrune
	for rows.Next() {
		value, err := scanPromotionPrune(rows.Scan)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// FinishPromotionPrune settles a pending request as pruned or kept, with the
// reason, and audits the decision in the same transaction. A settled request
// is never changed again.
func (s *Store) FinishPromotionPrune(ctx context.Context, promotionID, status, reason, batchID string) (model.PromotionPrune, error) {
	if status != model.PromotionPrunePruned && status != model.PromotionPruneKept {
		return model.PromotionPrune{}, fmt.Errorf("%w: promotion prune status %q", ErrInvalid, status)
	}
	if len(reason) > maximumPromotionPruneText {
		reason = reason[:maximumPromotionPruneText]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.PromotionPrune{}, fmt.Errorf("begin promotion prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanPromotionPrune(tx.QueryRowContext(ctx, `SELECT `+promotionPruneColumns+` FROM promotion_prunes WHERE promotion_id = ?`, promotionID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return model.PromotionPrune{}, fmt.Errorf("%w: promotion prune", ErrNotFound)
	}
	if err != nil {
		return model.PromotionPrune{}, err
	}
	if current.Status != model.PromotionPrunePending {
		return current, nil
	}
	now := unixNano(s.now())
	if _, err := tx.ExecContext(ctx, `UPDATE promotion_prunes SET status = ?, reason = ?, batch_id = ?, updated_at = ? WHERE promotion_id = ? AND status = 'pending'`,
		status, reason, batchID, now, promotionID); err != nil {
		return model.PromotionPrune{}, fmt.Errorf("finish promotion prune: %w", err)
	}
	details, err := json.Marshal(map[string]any{"promotion_id": promotionID, "status": status, "reason": reason, "batch_id": batchID})
	if err != nil {
		return model.PromotionPrune{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: current.Actor, Action: "promotion.prune_source." + status,
		ResourceType: "promotion", ResourceID: promotionID, Details: string(details)}, now); err != nil {
		return model.PromotionPrune{}, fmt.Errorf("audit promotion prune: %w", err)
	}
	updated, err := scanPromotionPrune(tx.QueryRowContext(ctx, `SELECT `+promotionPruneColumns+` FROM promotion_prunes WHERE promotion_id = ?`, promotionID).Scan)
	if err != nil {
		return model.PromotionPrune{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.PromotionPrune{}, fmt.Errorf("commit promotion prune: %w", err)
	}
	return updated, nil
}
