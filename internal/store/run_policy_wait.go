package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

const PolicyRefreshWaitingReason = "upstream-policy-refresh-unavailable"

// SetRunPolicyWaiting is idempotent, preserves restart/retry lineage, and may
// annotate an active claimed run, including checks after preflight. A
// concurrent cancellation wins without losing its terminal reason.
func (s *Store) SetRunPolicyWaiting(ctx context.Context, id string, waiting bool) (model.Run, error) {
	current, err := s.Run(ctx, id)
	if err != nil {
		return current, err
	}
	reason := current.Reason
	suffix := "; " + PolicyRefreshWaitingReason
	if waiting {
		if reason == "" {
			reason = PolicyRefreshWaitingReason
		} else if reason != PolicyRefreshWaitingReason && !strings.HasSuffix(reason, suffix) {
			reason += suffix
		}
	} else if reason == PolicyRefreshWaitingReason {
		reason = ""
	} else {
		reason = strings.TrimSuffix(reason, suffix)
	}
	if reason == current.Reason {
		return current, nil
	}
	value, err := scanRun(s.db.QueryRowContext(ctx, `
UPDATE runs SET reason = ?, updated_at = ?
WHERE id = ? AND status = 'running' AND reason = ?
RETURNING `+runColumns, reason, unixNano(s.now()), id, current.Reason))
	if errors.Is(err, sql.ErrNoRows) {
		return s.Run(ctx, id)
	}
	return value, err
}
