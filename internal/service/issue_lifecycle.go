package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type IssueLifecycle interface {
	ActiveIssueLock(context.Context, int64) (*model.IssueLock, error)
	BreakIssueLock(context.Context, string, int64) error
	CloseIssueWithReason(context.Context, string, int64, string) (model.Issue, error)
}

// withIssueLock enriches only a previously authorized issue projection.
func (service *API) withIssueLock(ctx context.Context, issue model.Issue) (model.Issue, error) {
	if service.issueLifecycle == nil {
		return issue, nil
	}
	lock, err := service.issueLifecycle.ActiveIssueLock(ctx, issue.ID)
	if err != nil {
		return model.Issue{}, err
	}
	issue.Lock = lock
	return issue, nil
}

type issueCloseResult struct {
	ID     int64            `json:"id"`
	Closed bool             `json:"closed"`
	Error  string           `json:"error,omitempty"`
	Lock   *model.IssueLock `json:"lock,omitempty"`
}

func (service *API) issueLifecycleTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	if !actor.Admin {
		return nil, fmt.Errorf("%w: %s requires an admin uplink", ErrForbidden, name)
	}
	if service.issues == nil || service.issueLifecycle == nil {
		return nil, fmt.Errorf("%w: issue lifecycle", ErrUnavailable)
	}
	if name == "issue_unlock" {
		var args struct {
			ID int64 `json:"id"`
		}
		if err := decodeTool(raw, &args); err != nil {
			return nil, err
		}
		if _, err := service.issueGet(ctx, actor, args.ID); err != nil {
			return nil, err
		}
		if err := service.issueLifecycle.BreakIssueLock(ctx, actor.Identity, args.ID); err != nil {
			return nil, err
		}
		return map[string]any{"id": args.ID, "unlocked": true}, nil
	}
	var args struct {
		IDs    []int64 `json:"ids"`
		Reason string  `json:"reason"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	args.Reason = strings.TrimSpace(args.Reason)
	if len(args.IDs) < 1 || len(args.IDs) > 50 || args.Reason == "" || len(args.Reason) > 4096 {
		return nil, fmt.Errorf("%w: 1..50 unique positive IDs and a reason (1..4096 bytes) required", ErrInvalidInput)
	}
	seen := map[int64]bool{}
	for _, id := range args.IDs {
		if id <= 0 || seen[id] {
			return nil, fmt.Errorf("%w: IDs must be unique and positive", ErrInvalidInput)
		}
		seen[id] = true
	}
	results := make([]issueCloseResult, 0, len(args.IDs))
	for _, id := range args.IDs {
		row := issueCloseResult{ID: id}
		_, err := service.issueGet(ctx, actor, id)
		if err == nil {
			_, err = service.issueLifecycle.CloseIssueWithReason(ctx, actor.Identity, id, args.Reason)
		}
		var held *store.IssueLockHeldError
		switch {
		case err == nil:
			row.Closed = true
		case errors.Is(err, store.ErrNotFound):
			row.Error = "not_found"
		case errors.As(err, &held):
			row.Error = "lock_held"
			row.Lock = &held.Lock
		default:
			return nil, err
		}
		results = append(results, row)
	}
	return map[string]any{"results": results}, nil
}
