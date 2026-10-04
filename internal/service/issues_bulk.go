package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
)

func (service *API) issueGetMany(ctx context.Context, actor api.Actor, ids []int64) (api.IssueGetManyResponse, error) {
	response := api.IssueGetManyResponse{}
	if len(ids) < 1 || len(ids) > maximumIssuePage {
		return response, fmt.Errorf("%w: ids must contain between 1 and 50 issue IDs", ErrInvalidInput)
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return response, fmt.Errorf("%w: issue IDs must be positive and unique", ErrInvalidInput)
		}
		seen[id] = true
	}
	// Reserve a fallback for every ID before admitting any complete record.
	// Budget exhaustion never hides IDs, and later smaller records can still fit.
	response.Results = make([]api.IssueGetManyResult, len(ids))
	for i, id := range ids {
		response.Results[i] = api.IssueGetManyResult{ID: id, Error: "response_limit"}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return api.IssueGetManyResponse{}, err
	}
	used := len(encoded)
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return api.IssueGetManyResponse{}, err
		}
		issue, err := service.issueGet(ctx, actor, id)
		result := api.IssueGetManyResult{ID: id}
		switch {
		case errors.Is(err, store.ErrNotFound):
			result.Error = "not_found"
		case err != nil:
			return api.IssueGetManyResponse{}, err
		default:
			wire := wireIssue(issue)
			// Raw strings alone are a lower bound on JSON size. Avoid allocating
			// an encoded copy of an individually oversized stored body/title.
			if len(wire.Body)+len(wire.Title)+len(wire.Branch)+len(wire.Kind)+len(wire.State)+len(wire.CIOrigin)+len(wire.CIWorkID) > api.IssueGetManyMaximumBytes {
				continue
			}
			result.Issue = &wire
		}
		candidate, err := json.Marshal(result)
		if err != nil {
			return api.IssueGetManyResponse{}, err
		}
		fallback, err := json.Marshal(response.Results[i])
		if err != nil {
			return api.IssueGetManyResponse{}, err
		}
		delta := len(candidate) - len(fallback)
		if used+delta <= api.IssueGetManyMaximumBytes {
			response.Results[i] = result
			used += delta
		}
	}
	return response, nil
}
