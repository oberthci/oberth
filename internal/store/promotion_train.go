package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

func encodePromotionTrain(train *model.PromotionTrain) (string, error) {
	if train == nil {
		return "", nil
	}
	data, err := json.Marshal(train)
	return string(data), err
}

func promotionTrainKey(train *model.PromotionTrain) string {
	if train == nil {
		return ""
	}
	copyTrain := model.PromotionTrain{SkipConflicting: train.SkipConflicting}
	for _, source := range train.Sources {
		copyTrain.Sources = append(copyTrain.Sources, model.PromotionSource{SHA: strings.ToLower(source.SHA), Ref: source.Ref})
	}
	data, _ := json.Marshal(copyTrain)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validatePromotionTrain(spec model.PromotionSpec) error {
	train := spec.Train
	if train == nil {
		return nil
	}
	if len(train.Sources) == 0 || len(train.Sources) > model.MaxPromotionSources {
		return fmt.Errorf("%w: promotion train requires 1..%d sources", ErrInvalid, model.MaxPromotionSources)
	}
	if train.Sources[0].SHA != spec.SourceSHA || train.Sources[0].Ref != spec.SourceBranch || train.Sources[0].GrantID != spec.GrantID {
		return fmt.Errorf("%w: train anchor does not match first source", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, source := range train.Sources {
		sha := strings.ToLower(source.SHA)
		if !validOID(sha) || source.Ref == "" || seen[sha] || (source.GrantID == "") != (spec.GrantID == "") {
			return fmt.Errorf("%w: train sources require unique full SHAs, refs and consistent authority", ErrInvalid)
		}
		seen[sha] = true
		if spec.ResultSHA == "" && (source.Skipped || len(source.ConflictPaths) > 0 || train.TreeSHA != "") {
			return fmt.Errorf("%w: a new train cannot have resolved sources", ErrInvalid)
		}
	}
	if train.TreeSHA != "" && !validOID(train.TreeSHA) {
		return fmt.Errorf("%w: invalid train tree SHA", ErrInvalid)
	}
	return nil
}

func validateTrainResolution(before, after *model.PromotionTrain, result string) error {
	if before == nil || after == nil || promotionTrainKey(before) != promotionTrainKey(after) || len(before.Sources) != len(after.Sources) {
		return fmt.Errorf("%w: train source order and conflict policy are immutable", ErrInvalid)
	}
	accepted := 0
	for i, source := range after.Sources {
		if source.GrantID != before.Sources[i].GrantID || source.Skipped && (!after.SkipConflicting || len(source.ConflictPaths) == 0) || !source.Skipped && len(source.ConflictPaths) > 0 {
			return fmt.Errorf("%w: invalid train source resolution", ErrInvalid)
		}
		if !source.Skipped {
			accepted++
		}
	}
	if result != "" && (!validOID(after.TreeSHA) || accepted == 0) || result == "" && after.TreeSHA != "" {
		return fmt.Errorf("%w: train result requires an accepted source and tree SHA", ErrInvalid)
	}
	return nil
}

func promotionGrants(p model.Promotion, includeSkipped bool) []model.PromotionSource {
	if p.Train == nil {
		if p.GrantID == "" {
			return nil
		}
		return []model.PromotionSource{{SHA: p.SourceSHA, GrantID: p.GrantID}}
	}
	var grants []model.PromotionSource
	for _, source := range p.Train.Sources {
		if source.GrantID != "" && (includeSkipped || !source.Skipped) {
			grants = append(grants, source)
		}
	}
	return grants
}

func trainSkipReason(train *model.PromotionTrain) string {
	if train == nil {
		return ""
	}
	var parts []string
	for i, source := range train.Sources {
		if source.Skipped {
			parts = append(parts, fmt.Sprintf("skipped source %d (%s), conflicting paths %q", i+1, source.SHA, source.ConflictPaths))
		}
	}
	return strings.Join(parts, "; ")
}

func (s *Store) PlanPromotionTrain(ctx context.Context, id, previous, result string, train *model.PromotionTrain) (model.Promotion, error) {
	if train == nil {
		return model.Promotion{}, fmt.Errorf("%w: train plan is required", ErrInvalid)
	}
	return s.planPromotion(ctx, id, previous, result, train)
}

func recordTrainGrantInclusion(ctx context.Context, tx *sql.Tx, id string, train *model.PromotionTrain) error {
	if train == nil {
		return nil
	}
	for _, source := range train.Sources {
		if source.GrantID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE promotion_train_grants SET included=? WHERE promotion_id=? AND grant_id=?`, !source.Skipped, id, source.GrantID); err != nil {
			return err
		}
	}
	return nil
}

func samePromotionTrain(left, right *model.PromotionTrain) bool {
	a, _ := encodePromotionTrain(left)
	b, _ := encodePromotionTrain(right)
	return a == b
}
func promotionPlanAuditAction(result string) string {
	if result == "" {
		return "promotion.train.resolve"
	}
	return "promotion.pending"
}

func sameTrainGrants(left, right *model.PromotionTrain) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if len(left.Sources) != len(right.Sources) {
		return false
	}
	for i, source := range left.Sources {
		if source.GrantID != right.Sources[i].GrantID {
			return false
		}
	}
	return true
}
