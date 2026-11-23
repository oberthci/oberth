package gitcache

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
)

// CheckDefaultPush reads the forge's current default head only after the
// receive names its refs. Topic receives use the cached snapshot; a default
// update must extend the live forge head as well as the advertised local head.
// Publication still fences its own predecessor and confirms its own push.
func (s *ReceiveSource) CheckDefaultPush(ctx context.Context, update RefUpdate) error {
	ctx, cancel := s.scope(ctx)
	defer cancel()
	r, err := s.base.remote(ctx, s.event.Repository)
	if err != nil {
		return err
	}
	if update.Ref != r.DefaultBranch {
		return refuse("default push ref")
	}
	p, err := s.pushed.path(s.event.Repository)
	if err != nil {
		return err
	}
	head, late, err := s.base.admissionHead(ctx, p, r)
	if err != nil || late != nil {
		return err
	}
	return s.pushed.checkIntentAncestry(ctx, p, model.Publication{Repo: s.event.Repository, Ref: update.Ref, ObjectSHA: update.NewSHA, PreviousSHA: &head.ObjectSHA}, r.DefaultBranch)
}
