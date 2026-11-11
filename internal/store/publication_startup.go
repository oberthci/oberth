package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// VerifyStartupAuditState verifies the original, unmodified chain and pending
// intents, allowing only the exact historical publication.retry encoding.
// External continuity can therefore be verified before any repair is written.
// Runtime mutation gates always use the strict VerifyAuditState path.
func (s *Store) VerifyStartupAuditState(ctx context.Context) (model.AuditHead, error) {
	return s.withAuditReadSnapshot(ctx, func(tx *sql.Tx) (model.AuditHead, error) {
		head, err := s.verifyAuditChain(ctx, tx)
		if err != nil {
			return model.AuditHead{}, err
		}
		_, err = readStartupPublicationRepairs(ctx, tx)
		return head, err
	})
}
func readStartupPublicationRepairs(ctx context.Context, tx *sql.Tx) ([]model.Publication, error) {
	pending, err := readPendingPublications(ctx, tx)
	if err != nil {
		return nil, err
	}
	var repairs []model.Publication
	for _, pub := range pending {
		var action auditPublicationAction
		if err := tx.QueryRowContext(ctx, `SELECT actor,action,details FROM audit_actions WHERE resource_type='publication' AND resource_id=? ORDER BY id DESC LIMIT 1`, pub.ID).Scan(&action.actor, &action.action, &action.details); err != nil {
			return nil, fmt.Errorf("pending publication %s has no audit intent: %w", pub.ID, err)
		}
		strictErr := verifyPendingPublicationSnapshot([]model.Publication{pub}, map[string]auditPublicationAction{pub.ID: action})
		if strictErr == nil {
			continue
		}
		if action.action != "publication.retry" || action.actor != pub.Actor || strings.TrimSpace(pub.Actor) == "" || pub.PromotionID != "" || pub.PreviousKnown || pub.PreviousSHA != "" || pub.Attempts != 0 || !pub.RetryAfter.IsZero() {
			return nil, strictErr
		}
		expected, err := publicationAuditDetails(pub, "retrying failed publication "+pub.ID)
		if err != nil {
			return nil, err
		}
		if !publicationAuditDetailsMatch(action.details, expected) {
			return nil, strictErr
		}
		repairs = append(repairs, pub)
	}
	return repairs, nil
}
