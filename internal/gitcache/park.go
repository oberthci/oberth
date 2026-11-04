package gitcache

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ParkSource proves commit ancestry from a public park branch in this exact
// repository. Private promotion pins, tags and similarly named branches cannot
// establish parked provenance. The prefix has a literal namespace boundary.
func (c *Cache) ParkSource(ctx context.Context, input, sha, prefix string) (string, error) {
	if err := ValidateSHA(sha); err != nil {
		return "", err
	}
	if !strings.HasPrefix(prefix, "refs/heads/") || !strings.HasSuffix(prefix, "/") || ValidateBranch(strings.TrimSuffix(strings.TrimPrefix(prefix, "refs/heads/"), "/")) != nil {
		return "", errors.New("invalid park ref prefix")
	}
	_, repoPath, err := c.path(input)
	if err != nil {
		return "", err
	}
	lock := c.repoLock(repoPath)
	if err = lock.RLockContext(ctx); err != nil {
		return "", err
	}
	defer lock.RUnlock()
	if err = c.checkRepositoryActive(ctx, input); err != nil {
		return "", err
	}
	kind, err := c.execute(ctx, commandSpec{dir: repoPath, args: []string{"cat-file", "--batch-check=%(objecttype)"}, stdin: strings.NewReader(sha + "\n")}, true)
	if err != nil {
		return "", err
	}
	kind = strings.TrimSpace(kind)
	if kind == sha+" missing" {
		return "", nil
	}
	if kind != "commit" {
		return "", errors.New("park source must be a commit object")
	}
	output, err := c.capture(ctx, repoPath, "for-each-ref", "--count=1", "--format=%(refname)", "--contains="+sha, prefix)
	if err != nil {
		return "", fmt.Errorf("check park ancestry: %w", err)
	}
	ref := strings.TrimSpace(output)
	if ref == "" {
		return "", nil
	}
	if !strings.HasPrefix(ref, prefix) || ValidateBranch(strings.TrimPrefix(ref, "refs/heads/")) != nil {
		return "", errors.New("invalid park source ref")
	}
	return strings.TrimPrefix(ref, "refs/heads/"), nil
}
