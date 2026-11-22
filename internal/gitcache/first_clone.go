package gitcache

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/oberthci/oberth/internal/gitport"
)

// awaitClone waits within ctx for the first clone of repository, starting it
// when none runs. The clone runs under the cache lifetime and the background
// bound, so a caller whose deadline passes leaves it running and is told so;
// once it ends, the next caller sees the clone or starts a new one after a
// failure.
func (c *Cache) awaitClone(ctx context.Context, repository, p string) error {
	clone := c.startClone(repository, p) //nolint:contextcheck // The cache lifetime owns the clone, not the caller who waits for it.
	return c.waitClone(ctx, clone, p)
}

func (c *Cache) waitClone(ctx context.Context, clone *transfer, p string) error {
	result, ended := c.awaitTransfer(ctx, p, clone)
	if !ended {
		return &gitport.CloneUnfinishedError{Cause: context.Cause(ctx)}
	}
	return result.err
}

// startClone joins or starts the first clone of repository at p. A clone a
// caller left announces its refs when it ends, so schedule reconciliation
// reads the repository once it is there instead of at the next unrelated move.
func (c *Cache) startClone(repository, p string) *transfer {
	var registered int64
	run := func(ctx context.Context, _ *atomic.Bool) outcome {
		registered = c.registeredID(ctx, repository)
		return outcome{err: c.cloneOnce(ctx, repository, p)}
	}
	ended := func(ctx context.Context, result outcome, end ending) {
		if result.err == nil && end.left && sameRepository(registered, c.registeredID(ctx, repository)) {
			c.announceClone(ctx, repository)
		}
	}
	return c.joinTransfer(p, run, ended)
}

// sameRepository reports whether a transfer that started with catalogue ID
// start ends for the repository registered as end: registered at the end, and
// either under the ID it started with or registered only meanwhile (an
// upstream check clones before repo add). A repository removed meanwhile, or
// removed and added again under another ID, has no readers for this transfer.
func sameRepository(start, end int64) bool {
	return end != 0 && (start == 0 || start == end)
}

// registeredID is the repository's catalogue ID as the upstream names it; 0
// when it is not registered or cannot be read.
func (c *Cache) registeredID(ctx context.Context, repository string) int64 {
	remote, err := c.remote(ctx, repository)
	if err != nil {
		return 0
	}
	return remote.RepositoryID
}

// cloneOnce clones unless an earlier clone published the repository after its
// caller looked. One clone runs per path at a time, so it needs no init slot,
// and callers that check the layout meanwhile are not held behind it.
func (c *Cache) cloneOnce(ctx context.Context, repository, p string) error {
	if _, err := os.Stat(filepath.Join(p, "HEAD")); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return unavailable("initialize", err)
	}
	return c.clone(ctx, repository, p)
}

// announceClone tells the cache's readers about the refs a clone brought when
// the caller that wanted them had stopped waiting.
func (c *Cache) announceClone(ctx context.Context, repository string) {
	refs, err := c.ListRefs(ctx, repository)
	if err != nil {
		return
	}
	updates := make([]RefUpdate, 0, len(refs))
	for _, ref := range refs {
		updates = append(updates, RefUpdate{Ref: ref.Name, NewSHA: ref.ObjectSHA})
	}
	c.changed(ctx, repository, updates)
}
