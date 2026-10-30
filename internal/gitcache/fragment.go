package gitcache

import (
	"context"
	"fmt"
	"strings"
)

func (c *Cache) TagSHA(ctx context.Context, input, tag string) (string, error) {
	return c.resolveTagSHA(ctx, input, tag, true)
}

// TagObjectSHA retains annotated tag identity rather than peeling to a commit.
func (c *Cache) TagObjectSHA(ctx context.Context, input, tag string) (string, error) {
	return c.resolveTagSHA(ctx, input, tag, false)
}

func (c *Cache) resolveTagSHA(ctx context.Context, input string, tag string, peel bool) (string, error) {
	if err := ValidateTag(tag); err != nil {
		return "", err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return "", err
	}
	lock := c.repoLock(path)
	// Replay invokes exact-tag lookup inside a DB writer transaction. Never
	// wait on a receive holding the repository lock: unavailable is unknown.
	if !peel {
		if !lock.TryLock() {
			return "", fmt.Errorf("tag %s lookup is busy in %s", tag, repo)
		}
	} else {
		lock.Lock()
	}
	defer lock.Unlock()
	if !c.isBare(ctx, path) {
		return "", fmt.Errorf("repository %s is not cached", repo)
	}
	ref := "refs/tags/" + tag
	if peel {
		ref += "^{commit}"
	}
	output, err := c.capture(ctx, path, "rev-parse", "--verify", ref)
	if err != nil {
		return "", fmt.Errorf("tag %s not found in %s", tag, repo)
	}
	sha := strings.TrimSpace(output)
	if err := ValidateSHA(sha); err != nil {
		return "", err
	}
	return sha, nil
}
