package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
)

// receive_outbox: operator repair for a durable receive record that blocks
// its repository (#1490).

type ReceiveOutbox interface {
	PendingReceives() ([]gitcache.PendingReceive, error)
	InspectReceiveRecord(string) (gitcache.ReceiveRecord, error)
	RetryReceiveRecord(context.Context, string, string, gitcache.ReceiveHandler) (gitcache.ReceiveRecord, error)
	AbandonReceiveRecord(context.Context, string, string) (gitcache.ReceiveRecord, error)
}

// SetReceiveOutbox wires outbox repair once the receive handler exists.
// Call before serving requests.
func (service *API) SetReceiveOutbox(outbox ReceiveOutbox, handler gitcache.ReceiveHandler) {
	service.receiveOutbox, service.receiveHandler = outbox, handler
}

func (service *API) receiveOutboxTool(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if !actor.Admin || actor.Scoped() {
		return nil, refuse(ErrForbidden, "receive_outbox requires an unscoped admin uplink")
	}
	if service.receiveOutbox == nil {
		return nil, refuse(ErrUnavailable, "receive outbox")
	}
	var args struct {
		Action string `json:"action"`
		Repo   string `json:"repo"`
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	if args.Action != "list" && strings.TrimSpace(args.Repo) == "" {
		return nil, fmt.Errorf("%w: repo is required for %s", ErrInvalidInput, args.Action)
	}
	switch args.Action {
	case "list":
		pending, err := service.receiveOutbox.PendingReceives()
		if err != nil {
			return nil, err
		}
		return map[string]any{"records": pending}, nil
	case "show":
		return service.receiveOutbox.InspectReceiveRecord(args.Repo)
	case "retry":
		if service.receiveHandler == nil {
			return nil, refuse(ErrUnavailable, "receive handler")
		}
		record, inspectErr := service.receiveOutbox.InspectReceiveRecord(args.Repo)
		if inspectErr != nil {
			return nil, inspectErr
		}
		var result gitcache.ReceiveRecord
		err := auditedGitMutation(ctx, service.auditor, actor.Identity, "receive_outbox.retry", "receive", outboxAuditResource(record), outboxAuditDetails(record, ""),
			func() error {
				var retryErr error
				result, retryErr = service.receiveOutbox.RetryReceiveRecord(ctx, args.Repo, args.ID, service.receiveHandler)
				return retryErr
			})
		if err != nil {
			return nil, err
		}
		if service.deletionQueued != nil {
			service.deletionQueued()
		}
		return result, nil
	case "abandon":
		reason := strings.TrimSpace(args.Reason)
		if reason == "" || len(reason) > 512 {
			return nil, fmt.Errorf("%w: abandon requires a reason of 1..512 bytes", ErrInvalidInput)
		}
		record, inspectErr := service.receiveOutbox.InspectReceiveRecord(args.Repo)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if record.ID != "" && args.ID != record.ID {
			return nil, fmt.Errorf("%w: abandon requires id %q (from show) to name the exact record", ErrInvalidInput, record.ID)
		}
		var result gitcache.ReceiveRecord
		err := auditedGitMutation(ctx, service.auditor, actor.Identity, "receive_outbox.abandon", "receive", outboxAuditResource(record), outboxAuditDetails(record, reason),
			func() error {
				var abandonErr error
				result, abandonErr = service.receiveOutbox.AbandonReceiveRecord(ctx, args.Repo, args.ID)
				return abandonErr
			})
		if err != nil {
			return nil, err
		}
		return result, nil
	default:
		return nil, fmt.Errorf("%w: action must be list, show, retry or abandon", ErrInvalidInput)
	}
}

func outboxAuditResource(record gitcache.ReceiveRecord) string {
	if record.ID != "" {
		return record.ID
	}
	return record.Repo
}

func outboxAuditDetails(record gitcache.ReceiveRecord, reason string) map[string]any {
	refs := make([]string, 0, min(len(record.Updates), 50))
	for _, update := range record.Updates {
		if len(refs) == 50 {
			break
		}
		state := update.NewSHA
		if update.Deleted() {
			state = "deleted"
		}
		refs = append(refs, update.Ref+" "+update.OldSHA+" -> "+state)
	}
	details := map[string]any{"repo": record.Repo, "record_id": record.ID, "record_actor": record.Actor, "state": record.State,
		"next": record.Next, "updates": len(record.Updates), "refs": refs, "corrupt": record.Corrupt}
	if reason != "" {
		details["reason"] = reason
	}
	return details
}
