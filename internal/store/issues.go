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

const issueColumns = `
id, repo_id, kind, branch, title, body, state, occurrences,
ci_origin, ci_work_sequence, ci_work_id, created_at, updated_at, closed_at`

func (s *Store) UpsertCIIssue(ctx context.Context, actor string, repoID int64, branch, title, body string) (model.Issue, error) {
	if strings.TrimSpace(actor) == "" || repoID <= 0 || strings.TrimSpace(branch) == "" || strings.TrimSpace(title) == "" {
		return model.Issue{}, fmt.Errorf("%w: CI issue fields are invalid", ErrInvalid)
	}
	acquired := s.now().UTC()
	now := unixNano(acquired)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, fmt.Errorf("begin upsert CI issue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanIssue(tx.QueryRowContext(ctx, `
INSERT INTO issues(repo_id, kind, branch, title, body, state, occurrences, created_at, updated_at)
VALUES(?, 'ci', ?, ?, ?, 'open', 1, ?, ?)
ON CONFLICT(repo_id, branch) WHERE kind = 'ci' AND state = 'open' DO UPDATE SET
    title = excluded.title,
    body = excluded.body,
    occurrences = issues.occurrences + 1,
    updated_at = excluded.updated_at
RETURNING `+issueColumns, repoID, branch, title, body, now, now))
	if err != nil {
		return model.Issue{}, fmt.Errorf("upsert CI issue: %w", err)
	}
	if err := appendIssueAudit(ctx, tx, actor, "issue.ci.upsert", value.ID, map[string]any{"branch": branch, "occurrences": value.Occurrences}, now); err != nil {
		return model.Issue{}, err
	}
	expires := acquired.Add(IssueLockTTL)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO issue_locks(issue_id, owner, acquired_at, expires_at) VALUES(?, ?, ?, ?)
ON CONFLICT(issue_id) DO UPDATE SET
    owner = excluded.owner,
    acquired_at = excluded.acquired_at,
    expires_at = excluded.expires_at`, value.ID, actor, now, unixNano(expires)); err != nil {
		return model.Issue{}, fmt.Errorf("lock CI issue for pushing uplink: %w", err)
	}
	if err := appendIssueAudit(ctx, tx, actor, "issue.lock.acquire", value.ID, map[string]any{"expires_at": expires.Format(time.RFC3339Nano), "source": "ci_red"}, now); err != nil {
		return model.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit upsert CI issue: %w", err)
	}
	return value, nil
}

func (s *Store) CloseCIIssue(ctx context.Context, actor string, issueID int64, resolution string) (model.Issue, error) {
	resolution = strings.TrimSpace(resolution)
	if strings.TrimSpace(actor) == "" || issueID <= 0 || resolution == "" {
		return model.Issue{}, fmt.Errorf("%w: CI issue close fields are invalid", ErrInvalid)
	}
	closedAt := s.now().UTC()
	now := unixNano(closedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, fmt.Errorf("begin close CI issue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireIssueMutationLock(ctx, tx, issueID, actor, closedAt, false); err != nil {
		return model.Issue{}, err
	}
	value, err := scanIssue(tx.QueryRowContext(ctx, `
UPDATE issues SET state = 'closed', closed_at = ?, updated_at = ?,
    body = CASE WHEN body = '' THEN ? ELSE body || char(10) || char(10) || ? END
WHERE id = ? AND kind = 'ci' AND state = 'open'
RETURNING `+issueColumns, now, now, resolution, resolution, issueID))
	if err != nil {
		return model.Issue{}, translateNotFound("open CI issue", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM issue_locks WHERE issue_id = ?`, value.ID); err != nil {
		return model.Issue{}, fmt.Errorf("release closed CI issue lock: %w", err)
	}
	if err := appendIssueAudit(ctx, tx, actor, "issue.ci.close", value.ID, map[string]any{"branch": value.Branch, "resolution": resolution}, now); err != nil {
		return model.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit close CI issue: %w", err)
	}
	return value, nil
}

func (s *Store) CreateManualIssue(ctx context.Context, actor string, spec model.ManualIssueSpec) (model.Issue, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(spec.Title) == "" {
		return model.Issue{}, fmt.Errorf("%w: manual issue fields are invalid", ErrInvalid)
	}
	updatedAt := s.now().UTC()
	now := unixNano(updatedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, fmt.Errorf("begin create manual issue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanIssue(tx.QueryRowContext(ctx, `
INSERT INTO issues(repo_id, kind, title, body, state, occurrences, created_at, updated_at)
VALUES(NULL, 'manual', ?, ?, 'open', 1, ?, ?)
RETURNING `+issueColumns, spec.Title, spec.Body, now, now))
	if err != nil {
		return model.Issue{}, fmt.Errorf("create manual issue: %w", err)
	}
	if err := appendIssueAudit(ctx, tx, actor, "issue.create", value.ID, map[string]any{"kind": value.Kind}, now); err != nil {
		return model.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit create manual issue: %w", err)
	}
	return value, nil
}

func (s *Store) Issue(ctx context.Context, id int64) (model.Issue, error) {
	value, err := scanIssue(s.db.QueryRowContext(ctx, `SELECT `+issueColumns+` FROM issues WHERE id = ?`, id))
	if err != nil {
		return model.Issue{}, translateNotFound("issue", err)
	}
	return value, nil
}

// OpenCIIssue is the direct repo-and-branch lookup used by status. It avoids
// coupling correctness to a bounded issue-page scan.
func (s *Store) OpenCIIssue(ctx context.Context, repoID int64, branch string) (model.Issue, error) {
	if repoID <= 0 || strings.TrimSpace(branch) == "" {
		return model.Issue{}, fmt.Errorf("%w: repository and CI branch are required", ErrInvalid)
	}
	value, err := scanIssue(s.db.QueryRowContext(ctx, `
SELECT `+issueColumns+` FROM issues
WHERE repo_id = ? AND kind = 'ci' AND branch = ? AND state = 'open'`, repoID, branch))
	if err != nil {
		return model.Issue{}, translateNotFound("open CI issue", err)
	}
	return value, nil
}

func (s *Store) UpdateManualIssue(ctx context.Context, actor string, id int64, patch model.IssuePatch) (model.Issue, error) {
	return s.updateIssue(ctx, actor, id, patch, true)
}

// UpdateIssue applies the MCP-visible title/body patch to either issue kind
// while preserving CI ownership and projection metadata. Deletion remains
// manual-only, and CI closure continues through CloseCIIssue.
func (s *Store) UpdateIssue(ctx context.Context, actor string, id int64, patch model.IssuePatch) (model.Issue, error) {
	return s.updateIssue(ctx, actor, id, patch, false)
}

func (s *Store) updateIssue(ctx context.Context, actor string, id int64, patch model.IssuePatch, manualOnly bool) (model.Issue, error) {
	if strings.TrimSpace(actor) == "" || id <= 0 || (patch.Title == nil && patch.Body == nil && patch.State == nil) {
		return model.Issue{}, fmt.Errorf("%w: issue patch is empty", ErrInvalid)
	}
	if patch.Title != nil && strings.TrimSpace(*patch.Title) == "" {
		return model.Issue{}, fmt.Errorf("%w: issue title is empty", ErrInvalid)
	}
	if patch.State != nil && !patch.State.Valid() {
		return model.Issue{}, fmt.Errorf("%w: issue state is invalid", ErrInvalid)
	}

	updatedAt := s.now().UTC()
	now := unixNano(updatedAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, fmt.Errorf("begin update issue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	closing := patch.State != nil && *patch.State == model.IssueClosed
	if err := requireIssueMutationLock(ctx, tx, id, actor, updatedAt, !closing); err != nil {
		return model.Issue{}, err
	}
	updates := []string{"updated_at = ?"}
	args := []any{now}
	if patch.Title != nil {
		updates = append(updates, "title = ?")
		args = append(args, *patch.Title)
	}
	if patch.Body != nil {
		updates = append(updates, "body = ?")
		args = append(args, *patch.Body)
	}
	if patch.State != nil {
		updates = append(updates, "state = ?", "closed_at = CASE WHEN ? = 'closed' THEN COALESCE(closed_at, ?) ELSE NULL END")
		args = append(args, *patch.State, *patch.State, now)
	}
	args = append(args, id)
	query := `UPDATE issues SET ` + strings.Join(updates, ", ") + ` WHERE id = ?`
	if manualOnly {
		query += ` AND kind = 'manual'`
	}
	query += ` RETURNING ` + issueColumns
	value, err := scanIssue(tx.QueryRowContext(ctx, query, args...))
	if err != nil {
		return model.Issue{}, translateNotFound("issue", err)
	}
	if closing {
		if _, err := tx.ExecContext(ctx, `DELETE FROM issue_locks WHERE issue_id = ?`, value.ID); err != nil {
			return model.Issue{}, fmt.Errorf("release closed manual issue lock: %w", err)
		}
	}
	if err := appendIssueAudit(ctx, tx, actor, "issue.update", value.ID, map[string]any{"state": value.State}, now); err != nil {
		return model.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit update issue: %w", err)
	}
	return value, nil
}

func (s *Store) DeleteManualIssue(ctx context.Context, actor string, id int64) error {
	if strings.TrimSpace(actor) == "" || id <= 0 {
		return fmt.Errorf("%w: actor and issue are required", ErrInvalid)
	}
	deletedAt := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete manual issue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanIssue(tx.QueryRowContext(ctx, `SELECT `+issueColumns+` FROM issues WHERE id = ? AND kind = 'manual'`, id))
	if err != nil {
		return translateNotFound("manual issue", err)
	}
	if err := requireIssueMutationLock(ctx, tx, id, actor, deletedAt, false); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM issues WHERE id = ? AND kind = 'manual'`, id)
	if err != nil {
		return fmt.Errorf("delete manual issue: %w", err)
	}
	if err := requireChanged("manual issue", result); err != nil {
		return err
	}
	if err := appendIssueAudit(ctx, tx, actor, "issue.delete", value.ID, map[string]any{"title": value.Title}, unixNano(deletedAt)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete manual issue: %w", err)
	}
	return nil
}

func (s *Store) ListIssues(ctx context.Context, filter model.IssueListFilter) (model.IssuePage, error) {
	if filter.Kind != "" && !filter.Kind.Valid() {
		return model.IssuePage{}, fmt.Errorf("%w: issue kind is invalid", ErrInvalid)
	}
	if filter.State != "" && !filter.State.Valid() {
		return model.IssuePage{}, fmt.Errorf("%w: issue state is invalid", ErrInvalid)
	}
	limit := issuePageLimit(filter.Limit)
	rows, err := s.db.QueryContext(ctx, `
SELECT `+issueColumns+` FROM issues
WHERE (? = 0 OR repo_id = ?)
  AND (? = '' OR kind = ?)
  AND (? = '' OR state = ?)
  AND (? = 0 OR id < ?)
ORDER BY id DESC LIMIT ?`,
		filter.RepoID, filter.RepoID, filter.Kind, filter.Kind, filter.State, filter.State,
		filter.BeforeID, filter.BeforeID, limit+1)
	if err != nil {
		return model.IssuePage{}, fmt.Errorf("list issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var issues []model.Issue
	for rows.Next() {
		value, err := scanIssue(rows)
		if err != nil {
			return model.IssuePage{}, fmt.Errorf("scan issue: %w", err)
		}
		issues = append(issues, value)
	}
	if err := rows.Err(); err != nil {
		return model.IssuePage{}, fmt.Errorf("list issues: %w", err)
	}
	page := model.IssuePage{Issues: issues}
	if len(page.Issues) > limit {
		page.Issues = page.Issues[:limit]
		page.NextBefore = page.Issues[len(page.Issues)-1].ID
	}
	return page, nil
}

func issuePageLimit(limit int) int {
	if limit <= 0 || limit > defaultPageSize {
		return defaultPageSize
	}
	return limit
}

func appendIssueAudit(ctx context.Context, tx *sql.Tx, actor, action string, issueID int64, details any, createdAt int64) error {
	body, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode issue audit details: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: action, ResourceType: "issue",
		ResourceID: fmt.Sprint(issueID), Details: string(body),
	}, createdAt); err != nil {
		return fmt.Errorf("append issue audit action: %w", err)
	}
	return nil
}

func requireIssueMutationLock(
	ctx context.Context,
	tx *sql.Tx,
	issueID int64,
	actor string,
	now time.Time,
	renew bool,
) error {
	lock, err := scanIssueLock(tx.QueryRowContext(ctx, `
SELECT issue_id, owner, acquired_at, expires_at FROM issue_locks WHERE issue_id = ?`, issueID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load issue mutation lock: %w", err)
	}
	if !lock.ExpiresAt.After(now) {
		return nil
	}
	if lock.Owner != actor {
		return ErrLockHeld
	}
	if !renew {
		return nil
	}
	expires := now.Add(IssueLockTTL)
	if _, err := tx.ExecContext(ctx, `
UPDATE issue_locks SET expires_at = ? WHERE issue_id = ? AND owner = ?`,
		unixNano(expires), issueID, actor); err != nil {
		return fmt.Errorf("renew issue mutation lock: %w", err)
	}
	return appendIssueAudit(ctx, tx, actor, "issue.lock.renew", issueID,
		map[string]any{"expires_at": expires.Format(time.RFC3339Nano)}, unixNano(now))
}

func (s *Store) AcquireIssueLock(ctx context.Context, issueID int64, owner string) (model.IssueLock, error) {
	if issueID <= 0 || strings.TrimSpace(owner) == "" {
		return model.IssueLock{}, fmt.Errorf("%w: issue and lock owner are required", ErrInvalid)
	}
	acquired := s.now().UTC()
	expires := acquired.Add(IssueLockTTL)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.IssueLock{}, fmt.Errorf("begin acquire issue lock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanIssueLock(tx.QueryRowContext(ctx, `
INSERT INTO issue_locks(issue_id, owner, acquired_at, expires_at) VALUES(?, ?, ?, ?)
ON CONFLICT(issue_id) DO UPDATE SET
    owner = excluded.owner,
    acquired_at = excluded.acquired_at,
    expires_at = excluded.expires_at
WHERE issue_locks.expires_at <= ? OR issue_locks.owner = excluded.owner
RETURNING issue_id, owner, acquired_at, expires_at`,
		issueID, owner, unixNano(acquired), unixNano(expires), unixNano(acquired)))
	if errors.Is(err, sql.ErrNoRows) {
		return model.IssueLock{}, ErrLockHeld
	}
	if err != nil {
		return model.IssueLock{}, fmt.Errorf("acquire issue lock: %w", err)
	}
	if err := appendIssueAudit(ctx, tx, owner, "issue.lock.acquire", issueID, map[string]any{"expires_at": expires.Format(time.RFC3339Nano)}, unixNano(acquired)); err != nil {
		return model.IssueLock{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.IssueLock{}, fmt.Errorf("commit acquire issue lock: %w", err)
	}
	return value, nil
}

func (s *Store) RenewIssueLock(ctx context.Context, issueID int64, owner string) (model.IssueLock, error) {
	if issueID <= 0 || strings.TrimSpace(owner) == "" {
		return model.IssueLock{}, fmt.Errorf("%w: issue and lock owner are required", ErrInvalid)
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.IssueLock{}, fmt.Errorf("begin renew issue lock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanIssueLock(tx.QueryRowContext(ctx, `
UPDATE issue_locks SET expires_at = ?
WHERE issue_id = ? AND owner = ? AND expires_at > ?
RETURNING issue_id, owner, acquired_at, expires_at`,
		unixNano(now.Add(IssueLockTTL)), issueID, owner, unixNano(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return model.IssueLock{}, ErrLockNotOwned
	}
	if err != nil {
		return model.IssueLock{}, fmt.Errorf("renew issue lock: %w", err)
	}
	if err := appendIssueAudit(ctx, tx, owner, "issue.lock.renew", issueID, map[string]any{"expires_at": value.ExpiresAt.Format(time.RFC3339Nano)}, unixNano(now)); err != nil {
		return model.IssueLock{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.IssueLock{}, fmt.Errorf("commit renew issue lock: %w", err)
	}
	return value, nil
}

func (s *Store) ReleaseIssueLock(ctx context.Context, issueID int64, owner string) error {
	if issueID <= 0 || strings.TrimSpace(owner) == "" {
		return fmt.Errorf("%w: issue and lock owner are required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin release issue lock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanIssueLock(tx.QueryRowContext(ctx, `
DELETE FROM issue_locks WHERE issue_id = ? AND owner = ?
RETURNING issue_id, owner, acquired_at, expires_at`, issueID, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLockNotOwned
	}
	if err != nil {
		return fmt.Errorf("release issue lock: %w", err)
	}
	if err := appendIssueAudit(ctx, tx, owner, "issue.lock.release", issueID, map[string]any{"acquired_at": value.AcquiredAt.Format(time.RFC3339Nano)}, unixNano(s.now())); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit release issue lock: %w", err)
	}
	return nil
}

func scanIssue(row rowScanner) (model.Issue, error) {
	var value model.Issue
	var repoID sql.NullInt64
	var created, updated int64
	var closed sql.NullInt64
	if err := row.Scan(&value.ID, &repoID, &value.Kind, &value.Branch,
		&value.Title, &value.Body, &value.State, &value.Occurrences,
		&value.CIOrigin, &value.CIWorkSequence, &value.CIWorkID,
		&created, &updated, &closed); err != nil {
		return model.Issue{}, err
	}
	value.RepoID = repoID.Int64
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	value.ClosedAt = nullableTime(closed)
	return value, nil
}

func scanIssueLock(row rowScanner) (model.IssueLock, error) {
	var value model.IssueLock
	var acquired, expires int64
	if err := row.Scan(&value.IssueID, &value.Owner, &acquired, &expires); err != nil {
		return model.IssueLock{}, err
	}
	value.AcquiredAt, value.ExpiresAt = fromUnixNano(acquired), fromUnixNano(expires)
	return value, nil
}
