package gitcache

import (
	"context"
	"fmt"
	"strings"
)

func (c *Cache) TagSHA(ctx context.Context, input string, tag string) (string, error) {
	if err := ValidateTag(tag); err != nil {
		return "", err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return "", err
	}
	lock := c.repoLock(path)
	lock.Lock()
	defer lock.Unlock()
	if !c.isBare(ctx, path) {
		return "", fmt.Errorf("repository %s is not cached", repo)
	}
	output, err := c.capture(ctx, path, "rev-parse", "--verify", "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("tag %s not found in %s", tag, repo)
	}
	sha := strings.TrimSpace(output)
	if err := ValidateSHA(sha); err != nil {
		return "", err
	}
	return sha, nil
}
