package gitcache

import (
	"context"
	"fmt"
	"strings"
)

// RemoteRef reads one canonical upstream branch or tag ref without changing
// the cache. A missing ref is represented by exists=false; malformed or
// ambiguous output fails closed.
func (c *Cache) RemoteRef(ctx context.Context, input, ref string) (sha string, exists bool, err error) {
	if err := validatePublicationRef(ref); err != nil {
		return "", false, err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return "", false, err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if err := c.configureRemote(ctx, repo, path); err != nil {
		return "", false, err
	}
	output, err := c.capture(ctx, path, "ls-remote", "--refs", "upstream", ref)
	if err != nil {
		return "", false, fmt.Errorf("read upstream %s: %w", ref, err)
	}
	output = strings.TrimSpace(output)
	if output == "" {
		return "", false, nil
	}
	var found string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != ref || found != "" {
			return "", false, fmt.Errorf("upstream %s returned an ambiguous ref result", ref)
		}
		found = strings.ToLower(fields[0])
	}
	if err := ValidateSHA(found); err != nil {
		return "", false, fmt.Errorf("upstream %s object ID: %w", ref, err)
	}
	return found, true, nil
}

func validatePublicationRef(ref string) error {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return ValidateBranch(strings.TrimPrefix(ref, "refs/heads/"))
	case strings.HasPrefix(ref, "refs/tags/"):
		return ValidateTag(strings.TrimPrefix(ref, "refs/tags/"))
	default:
		return fmt.Errorf("gitcache: publication ref must be a branch or tag")
	}
}
