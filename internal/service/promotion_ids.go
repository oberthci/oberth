package service

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type promotionIDReader interface {
	ResolvePromotionID(context.Context, string) (model.Promotion, error)
}
type scopedPromotionIDReader interface {
	ResolvePromotionIDIn(context.Context, string, []int64) (model.Promotion, error)
}

func (service *API) resolveReadPromotionID(ctx context.Context, id string) (model.Promotion, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	padded := id
	if len(padded)%2 != 0 {
		padded += "0"
	}
	_, err := hex.DecodeString(padded)
	if len(id) < 8 || len(id) > 32 || err != nil {
		return model.Promotion{}, fmt.Errorf("%w: promotion ID must be 8 to 32 hexadecimal characters", ErrInvalidInput)
	}
	if resolver, ok := service.promotions.(promotionIDReader); ok {
		p, err := resolver.ResolvePromotionID(ctx, id)
		if errors.Is(err, store.ErrInvalid) {
			return model.Promotion{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
		return p, err
	}
	if len(id) != 32 {
		return model.Promotion{}, refuse(ErrUnavailable, "promotion ID resolution")
	}
	return service.promotions.Promotion(ctx, id)
}

func (promotions scopedPromotions) ResolvePromotionID(ctx context.Context, id string) (model.Promotion, error) {
	if resolver, ok := promotions.PromotionRepository.(scopedPromotionIDReader); ok {
		return resolver.ResolvePromotionIDIn(ctx, id, promotions.scope.repoIDs)
	}
	if len(id) != 32 {
		return model.Promotion{}, refuse(ErrUnavailable, "scoped promotion ID resolution")
	}
	return promotions.Promotion(ctx, id)
}
