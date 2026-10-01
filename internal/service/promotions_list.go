package service

import (
	"context"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
)

type promotionListArguments struct {
	Repo      string                `json:"repo"`
	SourceSHA string                `json:"source_sha"`
	Status    model.PromotionStatus `json:"status"`
	Before    int64                 `json:"before"`
	Limit     int                   `json:"limit"`
}

func (service *API) promotionList(ctx context.Context, args promotionListArguments) (api.PromotionListResponse, error) {
	if args.Before < 0 || args.Limit < 0 || args.Limit > 200 || (args.SourceSHA != "" && !gitoid.Valid(args.SourceSHA)) || (args.Status != "" && !args.Status.Valid()) {
		return api.PromotionListResponse{}, fmt.Errorf("%w: promotion list requires a full source SHA, known status and bounded positive cursor/limit", ErrInvalidInput)
	}
	if service.promotions == nil {
		return api.PromotionListResponse{}, fmt.Errorf("%w: promotion history", ErrUnavailable)
	}
	filter := model.PromotionListFilter{SourceSHA: args.SourceSHA, Status: args.Status, Before: args.Before, Limit: args.Limit}
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
	for _, p := range page.Promotions {
		response.Promotions = append(response.Promotions, api.PromotionListItem{
			ID: p.ID, Sequence: p.Sequence, RepoID: p.RepoID, SourceBranch: p.SourceBranch, SourceSHA: p.SourceSHA,
			TargetRef: p.TargetRef, ResultSHA: p.ResultSHA, RunID: p.RunID, Status: string(p.Status),
			CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return response, nil
}
