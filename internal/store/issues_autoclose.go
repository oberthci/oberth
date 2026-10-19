package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// CIBranchWasPublishedOrDeleted distinguishes a deleted upstream branch from
// a red branch that Oberth has never published. A received deletion also
// counts, even if the branch had no successful publication.
func (s *Store) CIBranchWasPublishedOrDeleted(ctx context.Context, repoID int64, branch string) (bool, error) {
	if repoID <= 0 || strings.TrimSpace(branch) == "" {
		return false, fmt.Errorf("%w: CI branch is required", ErrInvalid)
	}
	var published bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM publications
WHERE repo_id = ? AND ref_kind = 'branch' AND ref = ? AND status = 'delivered'
)`, repoID, branch).Scan(&published); err != nil {
		return false, fmt.Errorf("check delivered branch publication: %w", err)
	}
	if published {
		return true, nil
	}
	var outcome string
	err := s.db.QueryRowContext(ctx, `SELECT outcome FROM receive_events
WHERE repo_id = ? AND ref_kind = 'branch' AND ref = ?
ORDER BY created_at DESC, rowid DESC LIMIT 1`, repoID, branch).Scan(&outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check latest branch receive: %w", err)
	}
	return outcome == "deleted", nil
}

// AutoCloseCIIssue closes only the issue version the reconciler inspected.
// The default branch is guarded here as well as in the caller. Projecting
// existing failures to passed keeps old run completions from reopening the
// issue; a newer CI work sequence can still report a fresh regression.
func (s *Store) AutoCloseCIIssue(ctx context.Context, actor string, observed model.Issue, resolution string) (bool, error) {
	resolution = strings.TrimSpace(resolution)
	if strings.TrimSpace(actor) == "" || observed.ID <= 0 || observed.RepoID <= 0 || observed.Kind != model.IssueCI ||
		observed.State != model.IssueOpen || strings.TrimSpace(observed.Branch) == "" || resolution == "" {
		return false, fmt.Errorf("%w: CI issue auto-close fields are invalid", ErrInvalid)
	}
	now := unixNano(s.now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin CI issue auto-close: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
UPDATE issues SET state = 'closed', closed_at = ?, updated_at = ?,
    body = CASE WHEN body = '' THEN ? ELSE body || char(10) || char(10) || ? END
WHERE id = ? AND repo_id = ? AND kind = 'ci' AND state = 'open'
  AND branch = ? AND ci_origin = ? AND ci_work_sequence = ? AND ci_work_id = ?
  AND occurrences = ? AND updated_at = ?
  AND branch != (SELECT default_branch FROM repositories WHERE id = ?)
`, now, now, resolution, resolution, observed.ID, observed.RepoID, observed.Branch,
		observed.CIOrigin, observed.CIWorkSequence, observed.CIWorkID,
		observed.Occurrences, unixNano(observed.UpdatedAt), observed.RepoID)
	if err != nil {
		return false, fmt.Errorf("close stale CI issue: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read CI issue auto-close result: %w", err)
	}
	if changed == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ci_issue_projections
SET outcome = 'passed', updated_at = ?
WHERE repo_id = ? AND branch = ? AND work_sequence <= ? AND outcome = 'failed'`,
		now, observed.RepoID, observed.Branch, observed.CIWorkSequence); err != nil {
		return false, fmt.Errorf("resolve stale CI projections: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM issue_locks WHERE issue_id = ?`, observed.ID); err != nil {
		return false, fmt.Errorf("release stale CI issue lock: %w", err)
	}
	if err := s.appendIssueAudit(ctx, tx, actor, "issue.ci.close", observed.ID,
		map[string]any{"branch": observed.Branch, "resolution": resolution}, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit CI issue auto-close: %w", err)
	}
	return true, nil
}
