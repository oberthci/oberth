package gitcache

import (
	"context"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
)

// DeliverTag puts an admitted release tag on the forge before any ref moves
// here, so the release run builds a tag the forge already has. It pushes the
// exact object this receive creates at that tag, peeled to the exact commit,
// through the installation's forge identity. A tag already on the forge at
// that object counts as delivered; a tag there at any other object is never
// moved. A delivered tag stays whatever the run does next.
func (s *ReceiveSource) DeliverTag(ctx context.Context, ref gitport.Ref) error {
	ctx, cancel := s.scope(ctx)
	defer cancel()
	if !strings.HasPrefix(ref.Name, "refs/tags/") || !slices.Contains(s.event.Updates, RefUpdate{Ref: ref.Name, NewSHA: ref.ObjectSHA}) {
		return refuse("immutable tag")
	}
	r := model.Publication{ID: "receive:" + s.event.ID, Actor: s.event.Actor, Repo: s.event.Repository, Ref: ref.Name, ObjectSHA: ref.ObjectSHA, CommitSHA: &ref.CommitSHA, PreviousSHA: new("")}
	return s.pushed.deliverTag(ctx, r)
}

func (c *Cache) deliverTag(ctx context.Context, r model.Publication) error {
	if err := validatePublication(r); err != nil {
		return err
	}
	p, err := c.path(r.Repo)
	if err != nil {
		return err
	}
	remote, err := c.remote(ctx, r.Repo)
	if err != nil {
		return err
	}
	held, err := c.forgeHolds(ctx, p, remote, r)
	if err != nil || held {
		return err
	}
	if err = c.object(ctx, p, r.ObjectSHA); err != nil {
		return err
	}
	peeled, err := c.peelRemote(ctx, p, gitport.Ref{Name: r.Ref, ObjectSHA: r.ObjectSHA})
	if err != nil {
		return err
	}
	if peeled.CommitSHA != *r.CommitSHA {
		return &RefusalError{reason: reasonPushCommit, remedy: remedyPushCommit}
	}
	return c.pushIntent(ctx, p, remote, r)
}
