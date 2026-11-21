package gitcache

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/gitport"
)

// firstCloneBound is how long the first clone of a repository may run. It is
// separate from the caller's deadline (a request or a schedule sync allows
// seconds): thirty minutes carries a few gigabytes over a modest link, and the
// idle budget still ends a clone whose transfer stops moving.
const firstCloneBound = 30 * time.Minute

// firstClone is one running first clone; done closes when err is final.
type firstClone struct {
	done chan struct{}
	err  error
	// left records that a caller stopped waiting before the clone ended.
	left bool
}

// firstClones holds the first clones still running, at most one per path,
// and lets Close wait for them.
type firstClones struct {
	mu      sync.Mutex
	running map[string]*firstClone
	wait    sync.WaitGroup
}

// awaitClone waits within ctx for the first clone of repository, starting it
// when none runs. The clone runs under the cache lifetime and firstCloneBound,
// so a caller whose deadline passes leaves it running and is told so; once it
// ends, the next caller sees the clone or starts a new one after a failure.
func (c *Cache) awaitClone(ctx context.Context, repository, p string) error {
	clone := c.startClone(repository, p) //nolint:contextcheck // The cache lifetime owns the clone, not the caller who waits for it.
	select {
	case <-clone.done:
		return clone.err
	case <-ctx.Done():
		c.clones.mu.Lock()
		clone.left = true
		c.clones.mu.Unlock()
		return &gitport.CloneUnfinishedError{Cause: context.Cause(ctx)}
	}
}

func (c *Cache) startClone(repository, p string) *firstClone {
	c.clones.mu.Lock()
	defer c.clones.mu.Unlock()
	if clone := c.clones.running[p]; clone != nil {
		return clone
	}
	clone := &firstClone{done: make(chan struct{})}
	c.clones.running[p] = clone
	c.clones.wait.Go(func() {
		ctx, cancel := context.WithTimeout(c.ctx, c.budgets.firstClone)
		defer cancel()
		registered := c.registeredID(ctx, repository)
		err := c.cloneOnce(ctx, repository, p)
		c.clones.mu.Lock()
		delete(c.clones.running, p)
		clone.err = err
		left := clone.left
		c.clones.mu.Unlock()
		close(clone.done)
		if err == nil && left && registered != 0 && c.registeredID(ctx, repository) == registered {
			c.announceClone(ctx, repository)
		}
	})
	return clone
}

// registeredID is the repository's catalogue ID as the upstream names it; 0
// when it is not registered or cannot be read. A clone announces only a
// repository registered under the same ID from its start to its end: a probe
// of an unregistered name or a repository removed meanwhile has no readers.
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
// the caller that wanted them had stopped waiting, so schedule reconciliation
// reads the repository once it is there instead of at the next unrelated move.
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
