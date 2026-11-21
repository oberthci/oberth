package gitcache

import (
	"context"
	"strings"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

// Record the confirmed publication after any older in-flight forge snapshot.
// Independent publications keep their network work outside the shared gate.
func (c *Cache) publishCached(ctx context.Context, p string, remote Remote, r model.Publication) error {
	if err := c.publishIntent(ctx, p, remote, r); err != nil {
		return err
	}
	slot := c.repositoryGate(p + "/fetch")
	if err := acquireContext(ctx, slot); err != nil {
		return err
	}
	defer func() { <-slot }()
	return c.recordPublication(ctx, p, r)
}

// The confirmed value is already known. Record it with its tracking ref in one
// recoverable transaction, before releasing promotion custody or reporting success.
func (c *Cache) recordPublication(ctx context.Context, p string, r model.Publication) error {
	if err := c.lock(ctx, p); err != nil {
		return err
	}
	defer c.unlock(p)
	public, err := c.refInventory(ctx, p)
	if err != nil {
		return err
	}
	var updates []RefUpdate
	// A completed publication retry preserves a newer accepted local push.
	if public[r.Ref] == r.ObjectSHA || public[r.Ref] == *r.PreviousSHA {
		if public[r.Ref] != r.ObjectSHA && receivePending(p) {
			pending := publicerr.New(publicerr.Unavailable, reasonPending, remedyPending, nil)
			pending.ID = gitport.ReceivePendingID
			return pending
		}
		updates = []RefUpdate{{Ref: r.Ref, OldSHA: public[r.Ref], NewSHA: r.ObjectSHA}}
	}
	if err = markReconciliation(p, updates); err != nil {
		return err
	}
	native := append([]RefUpdate{{Ref: "refs/oberth/upstream/" + strings.TrimPrefix(r.Ref, "refs/"), NewSHA: r.ObjectSHA}}, updates...)
	if err = c.refTransaction(ctx, p, native); err != nil {
		return err
	}
	return clearReconciliation(p)
}
