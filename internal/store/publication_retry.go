package store

import (
	"context"
	"database/sql"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// RetryPublication explicitly resets a failed or deferred attempt budget.
// It preserves exact object/predecessor and never reopens an abandoned intent.
func (s *Store) RetryPublication(ctx context.Context, action stateport.Action, input stateport.PublicationAttempt) error {
	return s.change(ctx, mutation{action.Actor, "publication.retry", "publication", input.ID, map[string]string{"reason": action.Reason}}, func(tx *sql.Tx) error {
		p, err := readPublication(ctx, tx, input.ID)
		if err != nil {
			return err
		}
		if p.Attempts != input.ExpectedAttempts {
			return refuse(attemptChanged, nil)
		}
		if p.Status != model.PublicationFailed && (p.Status != model.PublicationPending || p.RetryAt == nil) {
			return refuse(transition, nil)
		}
		p.Status = model.PublicationPending
		p.Attempted = true
		p.Attempts = 0
		p.RetryAt = nil
		p.Reason = ""
		p.UpdatedAt = s.options.Clock().UTC()
		return savePublication(ctx, tx, p)
	}, action.Events...)
}
