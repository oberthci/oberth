package gitcache

// This file is extracted verbatim from upstream PR #13 (commit 1e010341,
// "argoworkflow: resolve pinned cross-repo pipeline fragments before
// admission", author Mikita Zhuikou): ReadBlob is the bounded git-cache blob
// read the schedule feature depends on. Only the read primitive is taken;
// the fragment-resolution feature itself (TagSHA, fragment.go) is not
// integrated yet. When PR #13 lands, its copy of ReadBlob, validateBlobPath,
// errBlobTooLarge duplicates this file and one side must be
// dropped.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/boundedio"
)

var errBlobTooLarge = errors.New("blob exceeds the caller's limit")

func (c *Cache) ReadBlob(ctx context.Context, input, sha, file string, limit int) ([]byte, error) {
	if err := ValidateSHA(sha); err != nil {
		return nil, err
	}
	if err := validateBlobPath(file); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, errors.New("blob read limit must be positive")
	}
	repo, path, err := c.path(input)
	if err != nil {
		return nil, err
	}
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return nil, err
	}
	defer lock.RUnlock()
	if !c.isBare(ctx, path) {
		return nil, fmt.Errorf("repository %s is not cached", repo)
	}

	sink := &boundedio.Buffer{Limit: limit, LimitError: errBlobTooLarge}
	if err := c.run(ctx, commandSpec{
		dir:    path,
		args:   []string{"cat-file", "blob", sha + ":" + file},
		stdout: sink,
	}); err != nil {
		if errors.Is(sink.Err(), errBlobTooLarge) {
			return nil, fmt.Errorf("%s in %s at %s exceeds the %d byte limit", file, repo, sha, limit)
		}
		return nil, fmt.Errorf("read %s in %s at %s: %w", file, repo, sha, err)
	}
	if sink.Err() != nil {
		return nil, fmt.Errorf("read %s in %s at %s: %w", file, repo, sha, sink.Err())
	}
	return sink.Bytes(), nil
}

func validateBlobPath(file string) error {
	if file == "" {
		return errors.New("blob path is empty")
	}
	if file != strings.TrimSpace(file) {
		return fmt.Errorf("blob path %q has surrounding whitespace", file)
	}
	if strings.ContainsAny(file, "\x00\r\n\\") {
		return fmt.Errorf("blob path %q contains forbidden characters", file)
	}
	if strings.HasPrefix(file, "-") {
		return fmt.Errorf("blob path %q looks like an option", file)
	}
	if strings.HasPrefix(file, "/") {
		return fmt.Errorf("blob path %q must be relative to the repository root", file)
	}
	for _, segment := range strings.Split(file, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("blob path %q must not contain %q", file, segment)
		}
	}
	return nil
}
