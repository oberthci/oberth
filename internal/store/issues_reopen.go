package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// ReopenManualIssue preserves the issue and its history. CI state is owned by
// the run projection and must not be manufactured by a manual reopen request.
// The lock check, state change and audit append share one transaction.
func (s *Store) ReopenManualIssue(ctx context.Context, actor string, id int64) (model.Issue, error) {
	if strings.TrimSpace(actor) == "" || id <= 0 {
		return model.Issue{}, fmt.Errorf("%w: actor and issue are required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, fmt.Errorf("begin reopen manual issue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanIssue(tx.QueryRowContext(ctx, `SELECT `+issueColumns+` FROM issues WHERE id = ?`, id))
	if err != nil {
		return model.Issue{}, translateNotFound("issue", err)
	}
	if value.Kind != model.IssueManual {
		return model.Issue{}, fmt.Errorf("%w: only manual issues can be reopened; CI state follows runs", ErrInvalid)
	}
	now := s.now().UTC()
	if err := s.requireIssueMutationLock(ctx, tx, id, actor, now, value.State == model.IssueClosed); err != nil {
		return model.Issue{}, err
	}
	// A retry still checks ownership but does not rewrite timestamps or add a
	// second transition to the audit history.
	if value.State == model.IssueOpen {
		return value, nil
	}
	value, err = scanIssue(tx.QueryRowContext(ctx, `
UPDATE issues SET state = 'open', closed_at = NULL, updated_at = ?
WHERE id = ? AND kind = 'manual' AND state = 'closed'
RETURNING `+issueColumns, unixNano(now), id))
	if err != nil {
		return model.Issue{}, translateNotFound("closed manual issue", err)
	}
	if err := s.appendIssueAudit(ctx, tx, actor, "issue.reopen", id,
		map[string]any{"from": model.IssueClosed, "state": model.IssueOpen}, unixNano(now)); err != nil {
		return model.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit reopen manual issue: %w", err)
	}
	return value, nil
}
