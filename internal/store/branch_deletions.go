package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// Branch deletion records are the durable receipt for an accepted local
// branch deletion. The receive (or prune) path writes them quickly; upstream
// propagation happens later, outside every repository lock.

const branchDeletionColumns = `d.sequence, d.id, d.repo_id, r.name, d.branch, d.old_sha, d.actor, d.source,
d.batch_id, d.status, d.attempts, d.retry_after, d.error, d.created_at, d.updated_at`

const maximumBranchDeletionError = 2048

func insertBranchDeletionTx(ctx context.Context, tx *sql.Tx, id string, repoID int64, branch, oldSHA, actor, source, batchID string, now int64) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO branch_deletions(id, repo_id, branch, old_sha, actor, source, batch_id, status, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		id, repoID, branch, strings.ToLower(oldSHA), actor, source, batchID, now, now)
	if err != nil {
		return fmt.Errorf("insert branch deletion: %w", err)
	}
	return nil
}

// QueueBranchPrune records every planned deletion of one confirmed prune
// batch as pending before any local ref changes. Entries the local
// compare-and-delete refuses are marked skipped by FinishBranchPrune.
func (s *Store) QueueBranchPrune(ctx context.Context, batchID string, repoID int64, actor string, entries []model.BranchPruneEntry) error {
	if strings.TrimSpace(batchID) == "" || len(batchID) > 128 || repoID <= 0 || strings.TrimSpace(actor) == "" || len(entries) == 0 {
		return fmt.Errorf("%w: branch prune batch fields are invalid", ErrInvalid)
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Branch) == "" || !validOID(entry.SHA) {
			return fmt.Errorf("%w: branch prune entry is invalid", ErrInvalid)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin branch prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := unixNano(s.now())
	for index, entry := range entries {
		if err := insertBranchDeletionTx(ctx, tx, fmt.Sprintf("%s:%d", batchID, index), repoID, entry.Branch, entry.SHA, actor, model.BranchDeletionFromPrune, batchID, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit branch prune: %w", err)
	}
	return nil
}

// FinishBranchPrune marks the planned deletions the local compare-and-delete
// refused and appends the single audit entry for the batch.
func (s *Store) FinishBranchPrune(ctx context.Context, batchID, actor string, repoID int64, skipped map[string]string, details map[string]any) error {
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode branch prune audit: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin branch prune finish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := unixNano(s.now())
	for branch, reason := range skipped {
		if _, err := tx.ExecContext(ctx, `UPDATE branch_deletions SET status='skipped', error=?, updated_at=?
WHERE batch_id=? AND repo_id=? AND branch=? AND status='pending'`, boundedDeletionError(reason), now, batchID, repoID, branch); err != nil {
			return fmt.Errorf("skip branch prune entry: %w", err)
		}
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "branch.prune", ResourceType: "repository",
		ResourceID: fmt.Sprint(repoID), Details: string(encoded),
	}, now); err != nil {
		return fmt.Errorf("append branch prune audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit branch prune finish: %w", err)
	}
	return nil
}

// DueBranchDeletions returns pending records whose retry time has passed,
// oldest first.
func (s *Store) DueBranchDeletions(ctx context.Context, now time.Time, limit int) ([]model.BranchDeletion, error) {
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}
	rows, err := s.reader().QueryContext(ctx, `SELECT `+branchDeletionColumns+`
FROM branch_deletions d JOIN repositories r ON r.id=d.repo_id
WHERE d.status='pending' AND d.retry_after<=? ORDER BY d.sequence LIMIT ?`, unixNano(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list due branch deletions: %w", err)
	}
	return scanBranchDeletions(rows)
}

// BranchDeletions lists records newest first for receipts and status views.
func (s *Store) BranchDeletions(ctx context.Context, filter model.BranchDeletionFilter) ([]model.BranchDeletion, error) {
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	if filter.Status != "" && !filter.Status.Valid() {
		return nil, fmt.Errorf("%w: branch deletion status is invalid", ErrInvalid)
	}
	before := filter.Before
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := s.reader().QueryContext(ctx, `SELECT `+branchDeletionColumns+`
FROM branch_deletions d JOIN repositories r ON r.id=d.repo_id
WHERE (?=0 OR d.repo_id=?) AND (?='' OR d.status=?) AND d.sequence<?
ORDER BY d.sequence DESC LIMIT ?`, filter.RepoID, filter.RepoID, string(filter.Status), string(filter.Status), before, filter.Limit)
	if err != nil {
		return nil, fmt.Errorf("list branch deletions: %w", err)
	}
	return scanBranchDeletions(rows)
}

// BranchDeletionBatch reads every record of one receive event or prune batch.
func (s *Store) BranchDeletionBatch(ctx context.Context, batchID string) ([]model.BranchDeletion, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT `+branchDeletionColumns+`
FROM branch_deletions d JOIN repositories r ON r.id=d.repo_id
WHERE d.batch_id=? OR d.id=? ORDER BY d.sequence`, batchID, batchID)
	if err != nil {
		return nil, fmt.Errorf("read branch deletion batch: %w", err)
	}
	return scanBranchDeletions(rows)
}

// UpdateBranchDeletions applies one propagation pass. Rows that are no longer
// pending are left unchanged, so a stale worker cannot rewrite a final state.
func (s *Store) UpdateBranchDeletions(ctx context.Context, updates []model.BranchDeletionUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin branch deletion update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := unixNano(s.now())
	for _, update := range updates {
		if !update.Status.Valid() || update.Attempts < 0 {
			return fmt.Errorf("%w: branch deletion update is invalid", ErrInvalid)
		}
		retry := int64(0)
		if !update.RetryAfter.IsZero() {
			retry = unixNano(update.RetryAfter)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE branch_deletions SET status=?, attempts=?, retry_after=?, error=?, updated_at=?
WHERE id=? AND status='pending'`, string(update.Status), update.Attempts, retry, boundedDeletionError(update.Error), now, update.ID); err != nil {
			return fmt.Errorf("update branch deletion: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit branch deletion update: %w", err)
	}
	return nil
}

func boundedDeletionError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > maximumBranchDeletionError {
		return message[:maximumBranchDeletionError]
	}
	return message
}

func scanBranchDeletions(rows *sql.Rows) ([]model.BranchDeletion, error) {
	defer func() { _ = rows.Close() }()
	var result []model.BranchDeletion
	for rows.Next() {
		var value model.BranchDeletion
		var status string
		var retry, created, updated int64
		if err := rows.Scan(&value.Sequence, &value.ID, &value.RepoID, &value.Repository, &value.Branch, &value.OldSHA, &value.Actor,
			&value.Source, &value.BatchID, &status, &value.Attempts, &retry, &value.Error, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan branch deletion: %w", err)
		}
		value.Status = model.BranchDeletionStatus(status)
		if retry > 0 {
			value.RetryAfter = fromUnixNano(retry)
		}
		value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read branch deletions: %w", err)
	}
	return result, nil
}
