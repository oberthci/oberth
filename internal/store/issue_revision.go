package store

import (
	"context"
	"database/sql"
	"fmt"
)

// The caller holds the issue's writer transaction. A mismatch aborts that
// transaction, including any lock renewal prepared by its ownership check,
// before metadata changes can commit.
func requireIssueRevision(ctx context.Context, tx *sql.Tx, id int64, expected *int64) error {
	if expected == nil {
		return nil
	}
	if *expected < 1 {
		return fmt.Errorf("%w: expected_revision must be a positive integer", ErrInvalid)
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM issues WHERE id=?`, id).Scan(&revision); err != nil {
		return translateNotFound("issue", err)
	}
	if revision != *expected {
		return fmt.Errorf("%w: expected_revision mismatch; reread issue_get before retrying", ErrInvalidState)
	}
	return nil
}
