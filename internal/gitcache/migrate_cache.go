package gitcache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MigrateToQualifiedLayout renames flat cache directories (<root>/repo.git) to
// the org-qualified layout (<root>/upstream/org/repo.git) using the provided
// repo-to-qualification mapping.
//
// The migration is:
//   - Crash-safe: if interrupted mid-rename, the next startup completes it
//   - Lock-safe: holds the repo lock during rename
//   - Idempotent: already-migrated dirs are skipped
func (c *Cache) MigrateToQualifiedLayout(repos map[string]RepoQualification) error {
	for repo, qual := range repos {
		if qual.UpstreamName == "" || qual.Org == "" {
			continue // skip repos without full qualification
		}
		flatPath := filepath.Join(c.root, repo+".git")
		qualifiedPath := filepath.Join(c.root, qual.UpstreamName, qual.Org, repo+".git")

		if flatPath == qualifiedPath {
			continue // shouldn't happen, but guard
		}

		// Skip if flat dir doesn't exist (already migrated or never cached).
		if _, err := os.Stat(flatPath); os.IsNotExist(err) {
			continue
		}

		// Skip if qualified dir already exists (already migrated).
		if _, err := os.Stat(qualifiedPath); err == nil {
			continue
		}

		// Hold the repo lock during rename (locks key on the destination
		// path — the identity every post-migration access resolves to).
		lock := c.repoLock(qualifiedPath)
		lock.Lock()

		if err := c.migrateOneRepo(repo, flatPath, qualifiedPath); err != nil {
			lock.Unlock()
			return fmt.Errorf("migrate cache for %q: %w", repo, err)
		}

		lock.Unlock()
	}
	return nil
}

func (c *Cache) migrateOneRepo(repo, flatPath, qualifiedPath string) error {
	// Re-check after acquiring lock (another goroutine may have migrated).
	if _, err := os.Stat(flatPath); os.IsNotExist(err) {
		return nil // already gone
	}
	if _, err := os.Stat(qualifiedPath); err == nil {
		return nil // already exists at target
	}

	// Create the parent directory structure.
	parentDir := filepath.Dir(qualifiedPath)
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		return fmt.Errorf("create parent %q: %w", parentDir, err)
	}

	// Rename (atomic on the same filesystem).
	if err := os.Rename(flatPath, qualifiedPath); err != nil {
		return fmt.Errorf("rename %q -> %q: %w", flatPath, qualifiedPath, err)
	}

	c.logf("migrated cache: %s -> %s", flatPath, qualifiedPath)
	return nil
}

func (c *Cache) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
	}
}

// CacheRoot returns the cache root directory for external migration orchestration.
func (c *Cache) CacheRoot() string { return c.root }

// ListFlatCaches returns bare repository names that still use the flat
// (<root>/repo.git) layout. Used by migration verification.
func (c *Cache) ListFlatCaches() ([]string, error) {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, fmt.Errorf("read cache root: %w", err)
	}
	var flat []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".git") {
			continue
		}
		// A flat cache dir is directly under the root (not in a subdirectory).
		// Qualified dirs are under <root>/<upstream>/<org>/<repo>.git.
		bare := strings.TrimSuffix(name, ".git")
		if bare != "" && !strings.Contains(bare, "/") {
			flat = append(flat, bare)
		}
	}
	return flat, nil
}
