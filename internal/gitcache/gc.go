package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Compact explicitly consolidates a cache, bypassing automatic thresholds and
// attempt cooldown while retaining the same object-age safety window.
func (c *Cache) Compact(ctx context.Context, input string) error { return c.gc(ctx, input, false) }

// GC compacts only when the selected backend heuristics say work is useful,
// retaining the normal grace window for unreachable run objects.
func (c *Cache) GC(ctx context.Context, input string) error { return c.gc(ctx, input, true) }
func (c *Cache) gc(ctx context.Context, input string, automatic bool) error {
	_, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(path)
	lock.Lock()
	defer lock.Unlock()
	args := []string{"gc"}
	if automatic {
		args = append(args, "--auto")
	}
	return c.run(ctx, commandSpec{dir: path, args: args})
}

// GCAll compacts every validated bare cache under the root, scanning both the
// flat legacy layout (<root>/<repo>.git) and the qualified layout
// (<root>/<upstream>/<org>/<repo>.git) up to 3 levels deep.
func (c *Cache) GCAll(ctx context.Context) error {
	paths := c.discoverCachePaths()
	var errs []error
	for _, path := range paths {
		lock := c.repoLock(path)
		lock.Lock()
		bare := c.isBare(ctx, path)
		if bare {
			if err := c.run(ctx, commandSpec{dir: path, args: []string{"gc", "--auto"}}); err != nil {
				errs = append(errs, fmt.Errorf("gc %s: %w", path, err))
			}
		}
		lock.Unlock()
	}
	return errors.Join(errs...)
}

// discoverCachePaths finds all *.git directories under the cache root,
// scanning up to 3 levels deep to cover the flat layout (<root>/<repo>.git)
// and the qualified layout (<root>/<upstream>/<org>/<repo>.git).
func (c *Cache) discoverCachePaths() []string {
	var paths []string
	c.walkGitDirs(c.root, 3, &paths)
	return paths
}

func (c *Cache) walkGitDirs(dir string, depth int, paths *[]string) {
	if depth <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".git") {
			*paths = append(*paths, filepath.Join(dir, entry.Name()))
			continue
		}
		c.walkGitDirs(filepath.Join(dir, entry.Name()), depth-1, paths)
	}
}

// StartPeriodicGC runs until ctx ends. Call it from a process-owned goroutine.
func (c *Cache) StartPeriodicGC(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.GCAll(ctx); err != nil && !errors.Is(err, context.Canceled) {
				c.logger.Printf("periodic Git GC failed: %v", err)
			}
		}
	}
}
