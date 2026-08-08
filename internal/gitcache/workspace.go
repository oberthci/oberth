package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Checkout materializes an immutable detached workspace from the bare cache.
// The destination must not already exist.
func (c *Cache) Checkout(ctx context.Context, input, sha, destination string) error {
	if err := ValidateSHA(sha); err != nil {
		return err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return c.checkoutLocked(ctx, path, sha, destination)
}

func (c *Cache) checkoutLocked(ctx context.Context, source, sha, destination string) error {
	if destination == "" {
		return errors.New("workspace destination is required")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("workspace destination %s already exists", destination)
		}
		return fmt.Errorf("inspect workspace destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create workspace parent: %w", err)
	}
	if err := c.run(ctx, commandSpec{args: []string{"clone", "--quiet", "--no-checkout", "--no-hardlinks", source, destination}}); err != nil {
		return fmt.Errorf("clone cache into workspace: %w", err)
	}
	clean := false
	defer func() {
		if !clean {
			_ = os.RemoveAll(destination)
		}
	}()
	if err := c.run(ctx, commandSpec{dir: destination, args: []string{"fetch", "--quiet", "--force", "origin", sha}}); err != nil {
		return fmt.Errorf("fetch immutable workspace SHA: %w", err)
	}
	resolved, err := c.capture(ctx, destination, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("resolve immutable workspace SHA: %w", err)
	}
	if strings.TrimSpace(resolved) != sha {
		return fmt.Errorf("workspace resolved %s, expected %s", strings.TrimSpace(resolved), sha)
	}
	if err := c.run(ctx, commandSpec{dir: destination, args: []string{"checkout", "--quiet", "--detach", sha}}); err != nil {
		return fmt.Errorf("checkout immutable workspace SHA: %w", err)
	}
	clean = true
	return nil
}
