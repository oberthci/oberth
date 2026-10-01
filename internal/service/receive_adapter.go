package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/gitcache"
)

// ReceiveHandler is the narrow adapter passed to gitcache.Receive and
// ReplayPending. The durable gitcache event ID remains the Store idempotency
// key all the way through enqueue and audit.
type ReceiveHandler struct {
	Pushes *PushIngestor
}

func NewReceiveHandler(pushes *PushIngestor) (*ReceiveHandler, error) {
	if pushes == nil {
		return nil, errors.New("service: push ingestor is required")
	}
	return &ReceiveHandler{Pushes: pushes}, nil
}

func (handler *ReceiveHandler) HandleReceive(ctx context.Context, event gitcache.ReceiveEvent) error {
	_, err := handler.dispatch(ctx, event)
	return err
}

// dispatch routes one durable receive event to the push ingestor and returns
// the durable outcome so a live session can describe it to the pusher.
func (handler *ReceiveHandler) dispatch(ctx context.Context, event gitcache.ReceiveEvent) (PushResult, error) {
	ref := event.Update.Ref
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return handler.Pushes.Branch(ctx, BranchPush{
			EventID: event.ID, Repository: event.Repo, Branch: strings.TrimPrefix(ref, "refs/heads/"),
			OldOID: event.Update.OldSHA, NewOID: event.Update.NewSHA, Actor: event.Actor,
		})
	case strings.HasPrefix(ref, "refs/tags/"):
		return handler.Pushes.Tag(ctx, TagPush{
			EventID: event.ID, Repository: event.Repo, Tag: strings.TrimPrefix(ref, "refs/tags/"),
			OldOID: event.Update.OldSHA, NewOID: event.Update.NewSHA, Actor: event.Actor,
			ReleaseAdmissionSHA: event.ReleaseAdmission.SHA,
		})
	default:
		return PushResult{}, errors.New("service: receive event ref is outside heads/tags")
	}
}

// WithNotices returns a session-scoped receive handler that emits one
// human-readable line per durably processed ref update, for the pushing
// client's terminal. Notices describe only durable outcomes the pusher could
// equally read from the dashboard (queued run ID, recorded deletion, release
// admission or rejection reason) — never secret material. A crashed earlier
// receive replayed under this session's lock is described too; its lines
// state durable facts, so showing them to the current pusher stays truthful.
// Errors are not duplicated here: the SSH session error path already
// surfaces a failed delivery to the pusher.
func (handler *ReceiveHandler) WithNotices(notify func(format string, args ...any)) gitcache.ReceiveHandler {
	if notify == nil {
		return handler
	}
	return gitcache.ReceiveHandlerFunc(func(ctx context.Context, event gitcache.ReceiveEvent) error {
		result, err := handler.dispatch(ctx, event)
		if err != nil {
			return err
		}
		if notice := describePushResult(event, result); notice != "" {
			notify("%s", notice)
		}
		return nil
	})
}

func describePushResult(event gitcache.ReceiveEvent, result PushResult) string {
	kind := event.Update.Kind()
	name := strings.TrimPrefix(strings.TrimPrefix(event.Update.Ref, "refs/heads/"), "refs/tags/")
	target := event.Repo + "/" + name
	switch {
	case result.Deleted:
		return fmt.Sprintf("%s %s deletion recorded — no run", kind, name)
	case result.Run != nil:
		line := fmt.Sprintf("ref accepted · run %s queued (%s)", shortRunID(result.Run.ID), target)
		if kind == "tag" {
			line = fmt.Sprintf("release tag accepted · release run %s queued (%s)", shortRunID(result.Run.ID), target)
		}
		if result.Duplicate {
			line += " · replay of an already-recorded push"
		}
		if count := len(result.Cancellations); count > 0 {
			plural := ""
			if count != 1 {
				plural = "s"
			}
			line += fmt.Sprintf(" · superseded %d earlier run%s", count, plural)
		}
		return line
	case result.Reason != "":
		return fmt.Sprintf("%s %s: %s", kind, name, result.Reason)
	default:
		return fmt.Sprintf("%s %s recorded", kind, name)
	}
}

func shortRunID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

var _ gitcache.ReceiveHandler = (*ReceiveHandler)(nil)
