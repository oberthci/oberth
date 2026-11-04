package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// RecordRunIssueTrailers binds display metadata once, before execution completes.
// It never touches issue state, publication receipts or closure policy.
func (s *Store) RecordRunIssueTrailers(ctx context.Context, id, sha string, trailers []model.CommitIssueTrailer) error {
	if !validOID(sha) || len(trailers) > 256 {
		return fmt.Errorf("%w: run issue trailer binding", ErrInvalid)
	}
	if trailers == nil {
		trailers = []model.CommitIssueTrailer{}
	}
	for _, trailer := range trailers {
		if trailer.IssueID <= 0 || trailer.SHA != sha {
			return fmt.Errorf("%w: run issue trailer binding", ErrInvalid)
		}
	}
	body, err := json.Marshal(trailers)
	if err != nil {
		return err
	}
	if len(body) > 64<<10 {
		return fmt.Errorf("%w: run issue trailers exceed limit", ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET issue_trailers=? WHERE id=? AND sha=? AND issue_trailers='' AND status IN ('queued','running')`, string(body), id, sha)
	if err != nil {
		return fmt.Errorf("record run issue trailers: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// A retry of the exact immutable receipt is harmless; conflicting writes refuse.
		var existing string
		if err := s.reader().QueryRowContext(ctx, `SELECT issue_trailers FROM runs WHERE id=? AND sha=?`, id, sha).Scan(&existing); err != nil {
			return fmt.Errorf("read run issue trailer receipt: %w", err)
		}
		if existing != string(body) {
			return fmt.Errorf("%w: run issue trailers already recorded or run terminal", ErrInvalidState)
		}
	}
	return nil
}
