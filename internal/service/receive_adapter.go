package service

import (
	"context"
	"errors"
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
	ref := event.Update.Ref
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		_, err := handler.Pushes.Branch(ctx, BranchPush{
			EventID: event.ID, Repository: event.Repo, Branch: strings.TrimPrefix(ref, "refs/heads/"),
			OldOID: event.Update.OldSHA, NewOID: event.Update.NewSHA, Actor: event.Actor,
		})
		return err
	case strings.HasPrefix(ref, "refs/tags/"):
		_, err := handler.Pushes.Tag(ctx, TagPush{
			EventID: event.ID, Repository: event.Repo, Tag: strings.TrimPrefix(ref, "refs/tags/"),
			OldOID: event.Update.OldSHA, NewOID: event.Update.NewSHA, Actor: event.Actor,
			ReleaseAdmissionSHA: event.ReleaseAdmission.SHA,
		})
		return err
	default:
		return errors.New("service: receive event ref is outside heads/tags")
	}
}

var _ gitcache.ReceiveHandler = (*ReceiveHandler)(nil)
