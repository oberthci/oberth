package store

import (
	"context"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// InterruptedPromotionRetryEligible recognizes owner/transport interruptions,
// never manual cancellation, failed CI, publication, or trusted-preflight work.
// Admission repeats this check in its write transaction.
func (s *Store) InterruptedPromotionRetryEligible(ctx context.Context, id string) (bool, error) {
	promotion, err := s.Promotion(ctx, id)
	if err != nil {
		return false, err
	}
	eligible, err := interruptedPromotionRetryEligible(ctx, s.reader(), promotion)
	if err != nil || !eligible {
		return eligible, err
	}
	// A delivered tuple needs no startup retry. The admission transaction
	// separately classifies a delivery racing this read.
	err = s.reader().QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM promotions
 WHERE repo_id=? AND lower(source_sha)=lower(?) AND target_ref=? AND train_key=? AND status='passed')`, promotion.RepoID, promotion.SourceSHA, promotion.TargetRef, promotionTrainKey(promotion.Train)).Scan(&eligible)
	return eligible, err
}

func interruptedPromotionRetryEligible(ctx context.Context, queryer auditQueryer, promotion model.Promotion) (bool, error) {
	if promotion.Status != model.PromotionInterrupted {
		return false, nil
	}
	switch promotion.Error {
	case "oberth restarted during promotion CI", "oberth restarted before promotion publication intent", "job not in terminal state at restart; result unrecoverable", "scheduler stopped":
	default:
		if !strings.HasPrefix(promotion.Error, "job state unreadable at restart: ") {
			return false, nil
		}
	}
	var eligible bool
	err := queryer.QueryRowContext(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM publications WHERE promotion_id=?)
 AND (?='' OR EXISTS(SELECT 1 FROM runs WHERE id=? AND status='interrupted' AND phase='interrupted'))
 AND NOT EXISTS(SELECT 1 FROM step_results WHERE run_id=? AND status='failed')
 AND NOT EXISTS(SELECT 1 FROM run_cancellations WHERE run_id=? AND completed_at IS NULL)`, promotion.ID, promotion.RunID, promotion.RunID, promotion.RunID, promotion.RunID).Scan(&eligible)
	return eligible, err
}
