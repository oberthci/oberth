package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// GC asks Git to compact one repository only when its own heuristics say that
// work is useful. It does not aggressively prune run objects.
func (c *Cache) GC(ctx context.Context, input string) error {
	repo, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return c.run(ctx, commandSpec{dir: path, args: []string{"gc", "--auto"}})
}

// GCAll compacts every validated bare cache under the root.
func (c *Cache) GCAll(ctx context.Context) error {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return fmt.Errorf("list git caches: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".git") {
			continue
		}
		repo := strings.TrimSuffix(entry.Name(), ".git")
		if _, err := NormalizeRepo(repo); err != nil {
			continue
		}
		if err := c.GC(ctx, repo); err != nil {
			errs = append(errs, fmt.Errorf("gc %s: %w", repo, err))
		}
	}
	return errors.Join(errs...)
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
