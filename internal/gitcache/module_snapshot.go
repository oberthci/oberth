package gitcache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// All repository operations join the same fair queue. Context-aware readers
// can leave it without a detached goroutine acquiring the lock later.
func (c *Cache) lockModuleRepository(ctx context.Context, path string) (func(), error) {
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return nil, err
	}
	return lock.Unlock, nil
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

// ErrModuleSourceMissing means a pinned commit is positively absent locally.
// Retrying the same read cannot reconstruct a pruned object or rebind its tag.
var ErrModuleSourceMissing = errors.New("pinned module commit is not cached")

// Called with the repository lock held, only after a failed pinned read. Use
// cat-file's machine-readable missing response, never localized stderr or an
// exit status that could also denote a transient process/filesystem failure.
func (c *Cache) classifyModuleReadFailure(ctx context.Context, path, ref string, cause error) error {
	if ctx.Err() != nil || ValidateSHA(ref) != nil {
		return cause
	}
	query := ref + "^{commit}"
	output := &headBuffer{max: 256}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"}, stdin: strings.NewReader(query + "\n"), stdout: output}); err != nil {
		return cause
	}
	if strings.TrimSpace(string(output.data)) == query+" missing" {
		return fmt.Errorf("%w: %w", ErrModuleSourceMissing, cause)
	}
	return cause
}
