package service

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// recoverMovedPromotions runs the successor decision for a passed promotion
// whose publication was abandoned because its target moved and that has no
// retest: a stop between the abandonment and the recording of the retest left
// it so. It then gets its retest or fails with "retest not started"; a
// decision that fails is noted, never a reason for the server not to start.
// The scan reads abandoned publications, page by page.
func (s *Service) recoverMovedPromotions(ctx context.Context) error {
	q := stateport.RecordFilter{Status: string(model.PublicationAbandoned), Limit: stateport.RunPageLimit}
	for {
		rows, err := s.c.Publications.Publications(ctx, q)
		if err != nil {
			return err
		}
		for _, p := range rows {
			s.recoverMovedPromotion(ctx, p)
		}
		if len(rows) < q.Limit {
			return nil
		}
		q.Before = rows[len(rows)-1].ID
	}
}

// recoverMovedPromotion handles one row. Its failures, including a promotion
// that can no longer be read, are noted: one row never keeps the server from
// starting; only the scan's page read may fail the recovery.
func (s *Service) recoverMovedPromotion(ctx context.Context, p model.Publication) {
	if p.PromotionID == "" || p.Reason != reasonPubMoved {
		return
	}
	promotion, err := s.c.Promotions.Promotion(ctx, p.PromotionID)
	if err != nil {
		s.c.OnError(err)
		return
	}
	if promotion.Status != model.PromotionPassed {
		return
	}
	_, found, err := s.promotionSuccessor(ctx, promotion)
	if err == nil && !found {
		err = s.successor(ctx, promotion.ID)
	}
	if err != nil {
		s.c.OnError(err)
	}
}
