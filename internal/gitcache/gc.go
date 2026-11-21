package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// GC first restores every live owner's roots, then deletes orphan refs and
// compacts. Failed or incomplete owner reads and failed re-pins stop deletion;
// a live owner whose recorded source is gone stops it only in the
// repositories that owner records, so it never stops anyone else's.
func (c *Cache) GC(ctx context.Context) error {
	return c.withOwners(ctx, func(snapshot collection) error {
		paths, err := c.repositories()
		if err != nil {
			return unavailable("repositories", err)
		}
		var failures error
		for _, p := range paths {
			if err = c.collect(ctx, p, snapshot); err != nil {
				failures = errors.Join(failures, fmt.Errorf("repository %s: %w", p, err))
			}
		}
		return failures
	})
}
func (c *Cache) withOwners(ctx context.Context, visit func(collection) error) error {
	ctx, cancel := context.WithTimeout(ctx, c.budgets.local)
	defer cancel()
	if c.config.LiveOwners == nil {
		return refuse("live source owners")
	}
	epoch := c.beginCollection()
	defer c.endCollection()
	snapshot, err := c.restoreOwners(ctx)
	if err != nil {
		return err
	}
	snapshot.epoch = epoch
	return visit(snapshot)
}
func (c *Cache) refreshRetention(ctx context.Context, p string) error {
	if c.config.LiveOwners == nil {
		return nil
	}
	return c.withOwners(ctx, func(snapshot collection) error { return c.collect(ctx, p, snapshot) })
}

type collection struct {
	live    map[string]bool
	stalled map[string]error // cleaned repository path -> a live owner's gone source
	epoch   uint64
}

func (c *Cache) collect(ctx context.Context, p string, snapshot collection) error {
	if err := c.lock(ctx, p); err != nil {
		return err
	}
	defer c.unlock(p)
	if err := snapshot.stalled[filepath.Clean(p)]; err != nil {
		return err
	}
	active, err := c.restoreReceive(ctx, p, snapshot.live)
	if err != nil || active {
		return err
	}
	refs, err := c.keepRefs(ctx, p)
	if err != nil {
		return err
	}
	var names []string
	var tx strings.Builder
	expired := make(map[string]bool)
	tx.WriteString("start\n")
	for _, ref := range refs {
		parts := strings.Split(strings.TrimPrefix(ref, "refs/oberth/keep/"), "/")
		if len(parts) != 2 || !ValidOwner(parts[0]) {
			return refuse("keep ref")
		}
		if !c.ownerLive(parts[0], snapshot) {
			if c.expiredPromotion(parts[0]) {
				expired[parts[0]] = true
			}
			names = append(names, ref)
			fmt.Fprintf(&tx, "delete %s\n", ref)
		}
	}
	tx.WriteString("prepare\ncommit\n")
	if err = c.run(
		ctx,
		command{
			dir:   p,
			args:  []string{"update-ref", "--stdin"},
			input: strings.NewReader(tx.String()), refWrites: names, refDeletes: true,
		},
	); err != nil {
		return err
	}

	c.logExpirations(expired)
	return c.run(ctx, command{dir: p, args: []string{"gc", "--prune=now"}})
}
func (c *Cache) logExpirations(owners map[string]bool) {
	if c.config.Log == nil {
		return
	}
	for owner := range owners {
		c.config.Log(reasonPromotionExpired, owner)
	}
}

func (c *Cache) restoreOwners(ctx context.Context) (collection, error) {
	snapshot := collection{live: make(map[string]bool), stalled: make(map[string]error)}
	cursor := ""
	seen := make(map[string]bool)
	for {
		if ctx.Err() != nil {
			return collection{}, context.Cause(ctx)
		}
		page, err := c.config.LiveOwners(ctx, cursor)
		if err != nil {
			return collection{}, err
		}
		if len(page.Owners) > 128 {
			return collection{}, refuse("owner page")
		}
		for _, owner := range page.Owners {
			err = c.keepOwner(ctx, owner.ID, owner.Bindings)
			if err != nil && !sourceGone(err) {
				return collection{}, err
			}
			if err != nil {
				c.stall(snapshot, owner, err)
			}
			snapshot.live[owner.ID] = true
		}
		if page.Next == "" {
			return snapshot, nil
		}
		if seen[page.Next] {
			return collection{}, refuse("owner cursor")
		}
		seen[page.Next] = true
		cursor = page.Next
	}
}
// stall stops deletion in each repository an owner with gone source records.
func (c *Cache) stall(snapshot collection, owner Owner, cause error) {
	for repo := range bindingRoots(owner.Bindings) {
		if p, err := c.path(repo); err == nil {
			snapshot.stalled[filepath.Clean(p)] = cause
		}
	}
}
func (c *Cache) repositories() ([]string, error) {
	var paths []string
	err := filepath.WalkDir(c.config.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasSuffix(d.Name(), ".git") {
			paths = append(paths, p)
			return filepath.SkipDir
		}
		return nil
	})
	return paths, err
}

// Remove refuses retention and pending receive records before deleting a store.
func (c *Cache) Remove(ctx context.Context, repo string) error {
	p, err := c.path(repo)
	if err != nil {
		return err
	}
	if err = c.refreshRetention(ctx, p); err != nil {
		return err
	}
	if err = c.lock(ctx, p); err != nil {
		return err
	}
	defer c.unlock(p)
	if err = c.removable(ctx, p); err != nil {
		return err
	}
	return os.RemoveAll(p)
}

type RetainedError struct{ Owner string }

func (e *RetainedError) Error() string { return e.Reason() + ". " + e.Remedy() + " Owner: " + e.Owner }
func (*RetainedError) Reason() string  { return reasonRetained }
func (*RetainedError) Remedy() string  { return remedyRetained }

func (c *Cache) restoreReceive(ctx context.Context, p string, live map[string]bool) (bool, error) {
	record, err := readReservation(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if c.isActive("receive-" + record.Event.ID) {
		return true, nil
	}
	if record.State == "rejected" {
		return false, nil
	}
	_, roots, err := c.actualUpdates(ctx, p, record)
	if err != nil {
		return false, err
	}
	if record.Event.Admission.SHA != "" {
		roots = append(roots, record.Event.Admission.SHA)
	}
	if len(roots) > 0 {
		if err = c.keepRoots(ctx, p, "receive-"+record.Event.ID, keepRoots{objects: roots}); err != nil {
			return false, err
		}
	}
	live["receive-"+record.Event.ID] = true
	return false, nil
}

func (c *Cache) ownerLive(owner string, snapshot collection) bool {
	return c.livePromotion(owner) ||
		snapshot.live[owner] ||
		c.isActive(owner) ||
		c.keptDuringGC(owner, snapshot.epoch)
}
