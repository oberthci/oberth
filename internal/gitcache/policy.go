package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// ReadOptionalBlob distinguishes an absent path from unreadable Git state and
// refuses symlinks, trees and submodules. Policy callers must never interpret a
// failed object read as permission to omit a required check.
func (c *Cache) ReadOptionalBlob(ctx context.Context, input, sha, file string, limit int) ([]byte, bool, error) {
	if err := validatePolicyRead(sha, file, limit); err != nil {
		return nil, false, err
	}
	_, path, err := c.path(input)
	if err != nil {
		return nil, false, err
	}
	lock := c.repoLock(path)
	lock.Lock()
	defer lock.Unlock()
	return c.readOptionalBlobLocked(ctx, path, sha, file, limit)
}

// ReadDefaultBlob performs a successful fresh upstream fetch. Unlike Ensure,
// it has no stale-cache fallback, and reads the private upstream tracking ref,
// never the publicly pushable default branch in the local cache.
func (c *Cache) ReadDefaultBlob(ctx context.Context, input, file string, limit int) (string, []byte, bool, error) {
	if err := validatePolicyPath(file, limit); err != nil {
		return "", nil, false, err
	}
	_, path, err := c.path(input)
	if err != nil {
		return "", nil, false, err
	}
	lock := c.repoLock(path)
	lock.Lock()
	defer lock.Unlock()
	if !c.isBare(ctx, path) {
		return "", nil, false, errors.New("policy repository is not cached")
	}
	if err := c.configureRemote(ctx, input, path); err != nil {
		return "", nil, false, err
	}
	if err := c.fetchTracking(ctx, path); err != nil {
		return "", nil, false, fmt.Errorf("refresh required upstream policy: %w", err)
	}
	refs, err := c.capture(ctx, path, "for-each-ref", "--count=1", upstreamRefPrefix)
	if err != nil {
		return "", nil, false, err
	}
	if strings.TrimSpace(refs) == "" {
		return "", nil, false, nil // Successful fetch proved a genuinely unborn baseline.
	}
	branch, err := c.discoverDefaultBranch(ctx, path)
	if err != nil {
		return "", nil, false, err
	}
	sha, err := c.capture(ctx, path, "rev-parse", "--verify", upstreamRefPrefix+"heads/"+branch+"^{commit}")
	if err != nil {
		return "", nil, false, fmt.Errorf("resolve required upstream policy: %w", err)
	}
	sha = strings.TrimSpace(sha)
	if err := ValidateSHA(sha); err != nil {
		return "", nil, false, err
	}
	body, exists, err := c.readOptionalBlobLocked(ctx, path, sha, file, limit)
	return sha, body, exists, err
}

func validatePolicyRead(sha, file string, limit int) error {
	if err := ValidateSHA(sha); err != nil {
		return err
	}
	return validatePolicyPath(file, limit)
}

func validatePolicyPath(file string, limit int) error {
	if err := validateBlobPath(file); err != nil {
		return err
	}
	if limit < 1 || limit > 1<<20 {
		return errors.New("policy read limit must be 1..1048576 bytes")
	}
	return nil
}

func (c *Cache) readOptionalBlobLocked(ctx context.Context, path, sha, file string, limit int) ([]byte, bool, error) {
	// Literal pathspec prevents a policy filename from selecting multiple paths.
	entry := &headBuffer{max: 4096}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"ls-tree", "-z", sha, "--", ":(literal)" + file}, stdout: entry}); err != nil {
		return nil, false, fmt.Errorf("inspect required policy tree: %w", err)
	}
	if len(entry.data) == 0 {
		return nil, false, nil
	}
	records := bytes.Split(entry.data, []byte{0})
	if len(records) != 2 || len(records[1]) != 0 {
		return nil, false, errors.New("policy path did not resolve to one tree entry")
	}
	metadata, name, ok := strings.Cut(string(records[0]), "\t")
	fields := strings.Fields(metadata)
	if !ok || name != file || len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || ValidateSHA(fields[2]) != nil {
		return nil, false, errors.New("policy must be a regular Git blob")
	}
	body := &headBuffer{max: limit}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"cat-file", "blob", fields[2]}, stdout: body}); err != nil {
		return nil, false, fmt.Errorf("read required policy: %w", errors.Join(err, body.err))
	}
	return body.data, true, body.err
}
