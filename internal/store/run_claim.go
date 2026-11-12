package store

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
)

// ClaimNextRunMatching skips pools without preparation room without claiming
// their runs. accept must be a read-only, context-bounded admission check.
// Selection keeps the queue's priority and repository fair-share order; the
// write rechecks eligibility for the exact selected run, including admin holds.
func (s *Store) ClaimNextRunMatching(ctx context.Context, accept func(model.Run) bool) (model.Run, error) {
	const page = 64
	for offset := 0; ; offset += page {
		candidates, err := s.claimCandidates(ctx, page, offset)
		if err != nil {
			return model.Run{}, err
		}
		for _, run := range candidates {
			if err := ctx.Err(); err != nil {
				return model.Run{}, err
			}
			if accept(run) {
				return s.claimNextRun(ctx, run.ID)
			}
		}
		if len(candidates) < page {
			return model.Run{}, ErrNotFound
		}
	}
}

// Close each bounded page before routing reads: even a one-connection reader
// pool must remain available to resolve a candidate's durable target binding.
func (s *Store) claimCandidates(ctx context.Context, limit, offset int) ([]model.Run, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE `+claimableRunSQL+` AND `+automaticRetryReadySQL(s.now())+` AND `+admissionHoldAllowedSQL+` ORDER BY `+fairPriorityOrderSQL(s.now())+` LIMIT ? OFFSET ?`, unixNano(s.now()), limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var candidates []model.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, run)
	}
	return candidates, rows.Err()
}
