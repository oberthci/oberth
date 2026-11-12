package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

			Assist:  event.Assist,
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
	return &receiveNotices{dispatch: handler.dispatch, notify: notify}
}

type receiveNotices struct {
	dispatch func(context.Context, gitcache.ReceiveEvent) (PushResult, error)
	notify   func(string, ...any)
	runIDs   []string
}

func (handler *receiveNotices) HandleReceive(ctx context.Context, event gitcache.ReceiveEvent) error {
	result, err := handler.dispatch(ctx, event)
	if err != nil {
		return err
	}
	if result.Run != nil && result.Run.ID != "" && !slices.Contains(handler.runIDs, result.Run.ID) {
		handler.runIDs = append(handler.runIDs, result.Run.ID)
	}
	if notice := describePushResult(event, result); notice != "" {
		handler.notify("%s", notice)
	}
	return nil
}

// QueuedRunIDs exposes durable session results without parsing human notices.
func (handler *receiveNotices) QueuedRunIDs() []string { return slices.Clone(handler.runIDs) }

func describePushResult(event gitcache.ReceiveEvent, result PushResult) string {
	kind := event.Update.Kind()
	name := strings.TrimPrefix(strings.TrimPrefix(event.Update.Ref, "refs/heads/"), "refs/tags/")
	target := event.Repo + "/" + name
	switch {
	case result.Deleted:
		return fmt.Sprintf("%s %s deleted on the mirror · upstream deletion queued — no run", kind, name)
	case result.Run != nil:
		line := fmt.Sprintf("ref accepted · run %s queued (%s)", shortRunID(result.Run.ID), target)
		if kind == "tag" && result.Run.Release {
			line = fmt.Sprintf("release tag accepted · release run %s queued (%s)", shortRunID(result.Run.ID), target)
		}
		if kind == "tag" && !result.Run.Release {
			line = fmt.Sprintf("tag accepted · CI run %s queued (%s)", shortRunID(result.Run.ID), target)
		}
		if count := result.SupersededCount; count > 0 {
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
