package gitcache

import (
	"context"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
)

// maxReleaseTagBytes bounds the pusher-written tag object the forge receives.
const maxReleaseTagBytes = 64 << 10

// DeliverTag puts an admitted release tag on the forge before any ref moves
// here, so the release run builds a tag the forge already has. It pushes the
// exact object this receive creates at that tag: the commit itself, or one tag
// object directly on that commit, through the installation's forge identity.
// A tag already on the forge at that object counts as delivered; a tag there
// at any other object is never moved, and any forge rejection is final. Once
// its delivery has begun, the receive's admission waits for its outcome at the
// deadline instead of refusing.
func (s *ReceiveSource) DeliverTag(ctx context.Context, ref gitport.Ref) error {
	ctx, cancel := s.scope(ctx)
	defer cancel()
	if !strings.HasPrefix(ref.Name, "refs/tags/") || !slices.Contains(s.event.Updates, RefUpdate{Ref: ref.Name, NewSHA: ref.ObjectSHA}) {
		return refuse("immutable tag")
	}
	// Marked before the forge is touched: the gate, at its deadline, either sees
	// the mark and waits for this delivery's outcome, or ended first, and then
	// nothing is written.
	s.delivering.Store(true)
	if err := s.ctx.Err(); err != nil {
		return err
	}
	r := model.Publication{ID: "receive:" + s.event.ID, Actor: s.event.Actor, Repo: s.event.Repository, Ref: ref.Name, ObjectSHA: ref.ObjectSHA, CommitSHA: &ref.CommitSHA, PreviousSHA: new("")}
	if err := s.pushed.deliverTag(ctx, r); err != nil {
		return err
	}
	s.delivered.Store(true)
	return nil
}

func (c *Cache) deliverTag(ctx context.Context, r model.Publication) error {
	if err := validatePublication(r); err != nil {
		return err
	}
	p, err := c.path(r.Repo)
	if err != nil {
		return err
	}
	if err = c.releaseTagObject(ctx, p, r); err != nil {
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
	output, err := c.pushFenced(ctx, p, remote, r)
	if err == nil {
		// The forge accepted the create-only push; a read-back adds no certainty.
		return nil
	}
	return c.confirmPublication(ctx, publicationAttempt{store: p, remote: remote, intent: r, output: output, err: err, gate: true})
}

// releaseTagObject checks the pushed object locally: it peels to the admitted
// commit and is that commit or a tag object whose direct target is it.
func (c *Cache) releaseTagObject(ctx context.Context, p string, r model.Publication) error {
	if err := c.object(ctx, p, r.ObjectSHA); err != nil {
		return err
	}
	peeled, err := c.peelRemote(ctx, p, gitport.Ref{Name: r.Ref, ObjectSHA: r.ObjectSHA})
	if err != nil {
		return err
	}
	if peeled.CommitSHA != *r.CommitSHA {
		return &RefusalError{reason: reasonPushCommit, remedy: remedyPushCommit}
	}
	if r.ObjectSHA == *r.CommitSHA {
		return nil
	}
	data, err := c.capture(ctx, command{dir: p, args: []string{"cat-file", "tag", r.ObjectSHA}}, maxReleaseTagBytes)
	if err != nil {
		return &RefusalError{reason: reasonTagTarget, remedy: remedyTagTarget, Detail: err.Error()}
	}
	// Git reads a tag's target from its first two header lines and ignores later ones.
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) < 3 || lines[0] != "object "+*r.CommitSHA || lines[1] != "type commit" {
		return &RefusalError{reason: reasonTagTarget, remedy: remedyTagTarget}
	}
	return nil
}
