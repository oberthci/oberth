package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type promotionListArguments struct {
	Repo      string                `json:"repo"`
	SourceSHA string                `json:"source_sha"`
	Status    model.PromotionStatus `json:"status"`
	Before    pageCursor            `json:"before"`
	Limit     int                   `json:"limit"`
}

func (service *API) promotionList(ctx context.Context, args promotionListArguments) (api.PromotionListResponse, error) {
	// A scoped uplink pages by promotion ID: a sequence cursor would let it
	// probe the global admission count (#073).
	if args.Limit < 0 || args.Limit > 200 || (args.SourceSHA != "" && !gitoid.Valid(args.SourceSHA)) || (args.Status != "" && !args.Status.Valid()) ||
		service.scope != nil && args.Before.Sequence > 0 && args.Before.ID == "" {
		return api.PromotionListResponse{}, fmt.Errorf("%w: promotion list requires a full source SHA, known status and bounded positive cursor/limit", ErrInvalidInput)
	}
	if service.promotions == nil {
		return api.PromotionListResponse{}, fmt.Errorf("%w: promotion history", ErrUnavailable)
	}
	filter := model.PromotionListFilter{SourceSHA: args.SourceSHA, Status: args.Status, Before: args.Before.Sequence, Limit: args.Limit}
	if args.Before.ID != "" {
		cursor, err := service.promotions.Promotion(ctx, args.Before.ID)
		switch {
		case err == nil:
			filter.Before = cursor.Sequence
		case service.scope == nil && args.Before.Sequence > 0 && errors.Is(err, store.ErrNotFound):
		default:
			return api.PromotionListResponse{}, err
		}
	}
	if args.Repo != "" {
		repository, err := service.runs.RepositoryByName(ctx, args.Repo)
		if err != nil {
			return api.PromotionListResponse{}, err
		}
		filter.RepoID = repository.ID
	}
	page, err := service.promotions.ListPromotions(ctx, filter)
	if err != nil {
		return api.PromotionListResponse{}, err
	}
	response := api.PromotionListResponse{Promotions: []api.PromotionListItem{}, NextBefore: page.NextBefore}
	if service.scope != nil && page.NextBefore != 0 && len(page.Promotions) > 0 {
		response.NextCursor = page.Promotions[len(page.Promotions)-1].ID
	}
	for _, p := range page.Promotions {
		response.Promotions = append(response.Promotions, api.PromotionListItem{
			ID: p.ID, Sequence: p.Sequence, RepoID: p.RepoID, SourceBranch: p.SourceBranch, SourceSHA: p.SourceSHA,
			TargetRef: p.TargetRef, ResultSHA: p.ResultSHA, RunID: p.RunID, Status: string(p.Status),
			CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return response, nil
}
