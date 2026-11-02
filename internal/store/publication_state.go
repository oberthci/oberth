package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// PublicationState reads the outbox and its owners in one committed WAL
// snapshot. Delivery replies can be lost or stale; they are not status truth.
func (s *Store) PublicationState(ctx context.Context, id string) (model.PublicationFinalization, error) {
	tx, err := s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("begin publication status observation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	state := model.PublicationFinalization{}
	state.Publication, err = scanPublication(tx.QueryRowContext(ctx, `SELECT `+publicationColumns+` FROM publications WHERE id=?`, id))
	if err != nil {
		return state, translateNotFound("publication", err)
	}
	if state.Publication.RunID != "" {
		state.Run, err = scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, state.Publication.RunID))
		if err != nil {
			return state, translateNotFound("publication run", err)
		}
	}
	if state.Publication.PromotionID != "" {
		state.Promotion, err = promotionTx(ctx, tx, state.Publication.PromotionID)
		if err != nil {
			return state, err
		}
	}
	if err = tx.Commit(); err != nil {
		return model.PublicationFinalization{}, fmt.Errorf("finish publication status observation: %w", err)
	}
	return state, nil
}
