package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// QueuePriorityStore is an installation-wide admin capability. It is not
// exposed through scoped run dependencies.
type QueuePriorityStore interface {
	QueuePriorityPatterns(context.Context) ([]string, error)
	SetQueuePriorityPatterns(context.Context, string, []string) ([]string, error)
	BumpRunPriority(context.Context, string, string, model.RunPriority, string) (model.Run, error)
}

func (service *API) queuePriorityTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	if !actor.Admin {
		return nil, refuse(ErrForbidden, "queue priority requires an admin uplink")
	}
	if err := requireUnscoped(actor, name); err != nil {
		return nil, err
	}
	if service.queuePriorities == nil {
		return nil, refuse(ErrUnavailable, "queue priority")
	}
	if name == "queue_priority_get" {
		if err := decodeTool(raw, &struct{}{}); err != nil {
			return nil, err
		}
		patterns, err := service.queuePriorities.QueuePriorityPatterns(ctx)
		return map[string]any{"rc_patterns": patterns, "starvation_minutes": 60}, err
	}
	if name == "queue_priority_config" {
		var args struct {
			Patterns *[]string `json:"rc_patterns"`
		}
		if err := decodeTool(raw, &args); err != nil {
			return nil, err
		}
		if args.Patterns == nil {
			return nil, refuse(ErrInvalidInput, "rc_patterns is required; use [] to disable")
		}
		patterns, err := service.queuePriorities.SetQueuePriorityPatterns(ctx, actor.Identity, *args.Patterns)
		if err != nil {
			return nil, err
		}
		if err := service.notifyPriorityChange(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"rc_patterns": patterns, "starvation_minutes": 60}, nil
	}
	var args struct {
		RunID    string            `json:"id"`
		Priority model.RunPriority `json:"priority"`
		Reason   string            `json:"reason"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	history := service.history
	if history == nil {
		history, _ = service.runs.(RunHistory)
	}
	if history == nil {
		return nil, refuse(ErrUnavailable, "run ID resolution")
	}
	selected, err := history.ResolveRunID(ctx, args.RunID)
	if err != nil {
		return nil, err
	}
	args.RunID = selected.ID
	bump := service.queuePriorities.BumpRunPriority
	if guarded, ok := service.enqueues.(interface {
		BumpRunPriority(context.Context, string, string, model.RunPriority, string) (model.Run, error)
	}); ok {
		bump = guarded.BumpRunPriority
	}
	run, err := bump(ctx, actor.Identity, args.RunID, args.Priority, args.Reason)
	if err != nil {
		return nil, err
	}
	if err := service.notifyPriorityChange(ctx); err != nil {
		return nil, err
	}
	return service.presentationRun(ctx, run), nil
}

func (service *API) notifyPriorityChange(ctx context.Context) error {
	if refresher, ok := service.enqueues.(interface{ RefreshQueuePriorities(context.Context) error }); ok {
		if err := refresher.RefreshQueuePriorities(ctx); err != nil {
			return fmt.Errorf("priority saved; refresh waiting reservations: %w", err)
		}
	}
	service.signals.NotifyRun("")
	if service.enqueues != nil {
		service.enqueues.NotifyQueue()
	}
	return nil
}

const submissionPollInterval = 15 * time.Second
