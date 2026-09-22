package gitcache

// This file is extracted verbatim from upstream PR #13 (commit 1e010341,
// "argoworkflow: resolve pinned cross-repo pipeline fragments before
// admission", author Mikita Zhuikou): ReadBlob is the bounded git-cache blob
// read the schedule feature depends on. Only the read primitive is taken;
// the fragment-resolution feature itself (TagSHA, fragment.go) is not
// integrated yet. When PR #13 lands, its copy of ReadBlob, validateBlobPath,
// headBuffer, and errBlobTooLarge duplicates this file and one side must be
// dropped.

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	lock.Lock()
	defer lock.Unlock()
	if !c.isBare(ctx, path) {
		return nil, fmt.Errorf("repository %s is not cached", repo)
	}

	sink := &headBuffer{max: limit}
	if err := c.run(ctx, commandSpec{
		dir:    path,
		args:   []string{"cat-file", "blob", sha + ":" + file},
		stdout: sink,
	}); err != nil {
		if errors.Is(sink.err, errBlobTooLarge) {
			return nil, fmt.Errorf("%s in %s at %s exceeds the %d byte limit", file, repo, sha, limit)
		}
		return nil, fmt.Errorf("read %s in %s at %s: %w", file, repo, sha, err)
	}
	if sink.err != nil {
		return nil, fmt.Errorf("read %s in %s at %s: %w", file, repo, sha, sink.err)
	}
	return sink.data, nil
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

type headBuffer struct {
	data []byte
	max  int
	err  error
}

func (b *headBuffer) Write(value []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if len(b.data)+len(value) > b.max {
		b.err = errBlobTooLarge
		return 0, b.err
	}
	b.data = append(b.data, value...)
	return len(value), nil
}
