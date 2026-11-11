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
func (service *API) PrepareLongPoll(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (result api.PreparedLongPoll, resultErr error) {
	defer func() { resultErr = toolSelectorError(raw, resultErr) }()
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
			ID      string `json:"id"`
			Repo    string `json:"repo"`
			SHA     string `json:"sha"`
			Trigger string `json:"trigger"`
			timeoutArguments
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		timeout := arguments.seconds()
		if strings.TrimSpace(arguments.ID) == "" && strings.TrimSpace(arguments.SHA) == "" {
			return nil, fmt.Errorf("%w: id, or sha, is required", ErrInvalidInput)
		}
		var prepared func(context.Context) (WaitResponse, error)
		if arguments.ID != "" {
			if arguments.Repo != "" || arguments.SHA != "" || arguments.Trigger != "" {
				return nil, fmt.Errorf("%w: id cannot be combined with repo, sha, or trigger", ErrInvalidInput)
			}
			prepared, err = service.prepareWaitRunID(ctx, arguments.ID, timeout)
		} else {
			prepared, err = service.prepareWaitRun(ctx, arguments.Repo, arguments.SHA, arguments.Trigger, timeout, actor.Identity)
		}
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (any, error) {
			response, err := prepared(ctx)
			return service.stripScopedSequences(response), toolSelectorError(raw, err)
		}, nil
	case "promote_status":
		var arguments struct {
			ID string `json:"id"`
			timeoutArguments
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		timeout := arguments.seconds()
		prepared, err := service.prepareWaitPromotion(ctx, arguments.ID, timeout)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (any, error) {
			response, err := prepared(ctx)
			return service.stripScopedSequences(response), toolSelectorError(raw, err)
		}, nil
	case "run_wait":
		prepared, err := service.prepareRunWait(ctx, raw)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (any, error) {
			response, err := prepared(ctx)
			return response, toolSelectorError(raw, err)
		}, nil
	case "events_wait":
		prepared, err := service.prepareEventsWait(ctx, actor, raw)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (any, error) {
			response, err := prepared(ctx)
			return response, toolSelectorError(raw, err)
		}, nil
	default:
		return nil, fmt.Errorf("%w: tool is not an observation long poll", ErrInvalidInput)
	}
}
