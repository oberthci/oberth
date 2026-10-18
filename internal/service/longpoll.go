package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
)

// PrepareLongPoll validates and resolves the first observation before an HTTP
// stream starts. The returned single-use call consumes that observation instead
// of issuing an extra lookup. Cancellation stops
// only the observer; neither this method nor the call cancels durable work.
func (service *API) PrepareLongPoll(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (api.PreparedLongPoll, error) {
	if strings.TrimSpace(actor.Identity) == "" {
		return nil, errors.New("service: acting uplink identity is required")
	}
	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	switch name {
	case "wait":
		var arguments struct {
			Repo    string `json:"repo"`
			SHA     string `json:"sha"`
			Trigger string `json:"trigger"`
			Timeout int    `json:"timeout"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		prepared, err := service.prepareWaitRun(ctx, arguments.Repo, arguments.SHA, arguments.Trigger, arguments.Timeout, actor.Identity)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (any, error) {
			response, err := prepared(ctx)
			return service.redact(response), err
		}, nil
	case "promote_status":
		var arguments struct {
			ID      string `json:"id"`
			Timeout int    `json:"timeout"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		prepared, err := service.prepareWaitPromotion(ctx, arguments.ID, arguments.Timeout)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (any, error) {
			response, err := prepared(ctx)
			return service.redact(response), err
		}, nil
	default:
		return nil, fmt.Errorf("%w: tool is not an observation long poll", ErrInvalidInput)
	}
}
