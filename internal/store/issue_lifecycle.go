package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// IssueLockHeldError retains the stable sentinel and adds actionable public
// ownership metadata. Callers must resolve issue visibility before mutation.
type IssueLockHeldError struct{ Lock model.IssueLock }

func (err *IssueLockHeldError) Error() string {
	return fmt.Sprintf("%s: owner %q until %s", ErrLockHeld, err.Lock.Owner, err.Lock.ExpiresAt.UTC().Format(time.RFC3339Nano))
}
func (err *IssueLockHeldError) Unwrap() error { return ErrLockHeld }

// ActiveIssueLock observes transient ownership without changing issue state.
// API callers resolve issue visibility before using this reader.
func (s *Store) ActiveIssueLock(ctx context.Context, id int64) (*model.IssueLock, error) {
	lock, err := scanIssueLock(s.reader().QueryRowContext(ctx, `SELECT issue_id,owner,acquired_at,expires_at FROM issue_locks WHERE issue_id=? AND expires_at>?`, id, unixNano(s.now())))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lock, nil
}

// BreakIssueLock is the administrative override. The service checks admin and
// scope; removal and prior-owner attribution commit together.
func (s *Store) BreakIssueLock(ctx context.Context, actor string, id int64) error {
	if id <= 0 || strings.TrimSpace(actor) == "" {
		return fmt.Errorf("%w: actor and issue required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE id=?)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	lock, err := scanIssueLock(tx.QueryRowContext(ctx, `DELETE FROM issue_locks WHERE issue_id=? RETURNING issue_id,owner,acquired_at,expires_at`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.appendIssueAudit(ctx, tx, actor, "issue.lock.break", id, map[string]any{"previous_owner": lock.Owner, "acquired_at": lock.AcquiredAt, "expires_at": lock.ExpiresAt}, unixNano(s.now())); err != nil {
		return err
	}
	return tx.Commit()
}

// CloseIssueWithReason applies each bulk item independently, retaining locks
// and recording the reason in both the body and the same transaction's audit.
func (s *Store) CloseIssueWithReason(ctx context.Context, actor string, id int64, reason string) (model.Issue, error) {
	reason = strings.TrimSpace(reason)
	if id <= 0 || strings.TrimSpace(actor) == "" || reason == "" || len(reason) > 4096 {
		return model.Issue{}, fmt.Errorf("%w: actor, issue and reason (1..4096 bytes) required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC()
	if err := s.requireIssueMutationLock(ctx, tx, id, actor, now, false); err != nil {
		return model.Issue{}, err
	}
	existing, err := scanIssue(tx.QueryRowContext(ctx, `SELECT `+issueColumns+` FROM issues WHERE id=?`, id))
	if err != nil {
		return model.Issue{}, translateNotFound("issue", err)
	}
	if existing.State == model.IssueClosed {
		return existing, nil
	}
	if existing.Kind == model.IssueCI {
		value, err := s.closeCIIssue(ctx, tx, actor, id, reason, now)
		if err != nil {
			return model.Issue{}, err
		}
		if err := tx.Commit(); err != nil {
			return model.Issue{}, err
		}
		return value, nil
	}
	value, err := scanIssue(tx.QueryRowContext(ctx, `UPDATE issues SET state='closed',closed_at=?,updated_at=?,body=CASE WHEN body='' THEN ? ELSE body||char(10)||char(10)||? END WHERE id=? RETURNING `+issueColumns, unixNano(now), unixNano(now), reason, reason, id))
	if err != nil {
		return model.Issue{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM issue_locks WHERE issue_id=?`, id); err != nil {
		return model.Issue{}, err
	}
	if err := s.appendIssueAudit(ctx, tx, actor, "issue.close", id, map[string]any{"reason": reason, "kind": value.Kind, "source": "issue_close_many"}, unixNano(now)); err != nil {
		return model.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, err
	}
	return value, nil
}
