package gitcache

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Proxy reads must be able to leave the repository queue at their deadline.
// Other Git operations retain their existing serialization; no detached waiter
// goroutine can take this mutex after its HTTP request has returned.
func (c *Cache) lockModuleRepository(ctx context.Context, path string) (func(), error) {
	lock := c.repoLock(path)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if lock.TryLock() {
			if err := ctx.Err(); err != nil {
				lock.Unlock()
				return nil, err
			}
			return lock.Unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// ModuleTagSnapshot resolves an exact public tag and its published timestamp
// under the same repository lock. The object ID pins subsequent show/archive
// reads even if a refresh changes the tag between operations.
func (c *Cache) ModuleTagSnapshot(ctx context.Context, input, tag string) (string, time.Time, error) {
	if err := ValidateTag(tag); err != nil {
		return "", time.Time{}, err
	}
	_, path, err := c.modulePath(ctx, input)
	if err != nil {
		return "", time.Time{}, err
	}
	unlock, err := c.lockModuleRepository(ctx, path)
	if err != nil {
		return "", time.Time{}, err
	}
	defer unlock()
	if !c.isBare(ctx, path) {
		return "", time.Time{}, errors.New("module repository is not cached")
	}
	ref := "refs/tags/" + tag
	output, err := c.capture(ctx, path, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(creatordate:iso-strict)", ref)
	if err != nil {
		return "", time.Time{}, err
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) != 3 || fields[0] != ref {
			continue
		}
		if err := ValidateSHA(fields[1]); err != nil {
			return "", time.Time{}, err
		}
		timestamp, err := time.Parse(time.RFC3339, fields[2])
		return fields[1], timestamp, err
	}
	return "", time.Time{}, errors.New("exact module tag is not cached")
}

func (c *Cache) modulePath(ctx context.Context, input string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if c.qualifyContext != nil {
		return c.pathWithQualifier(input, func(input string) (RepoQualification, error) { return c.qualifyContext(ctx, input) })
	}
	return c.path(input)
}
