package gitcache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrTagExists reports that refs/tags/<name> already names another commit,
// on the mirror or upstream. It is never transient.
var ErrTagExists = errors.New("tag already exists at another commit")

// PublishAnnotatedTag pushes an annotated tag on commit to the upstream
// without force, for promote tag_after (#1524), and returns the tag object.
// The tag object is created once on the mirror (tagger Oberth, the same
// identity as promotion merges) and reused by every retry; a mirror tag
// that already peels to commit is reused as it is. A tag at another commit
// is ErrTagExists. The remote ref is verified after the push.
func (c *Cache) PublishAnnotatedTag(ctx context.Context, input, tag, commit, message string) (string, error) {
	if err := ValidateTag(tag); err != nil {
		return "", err
	}
	if err := ValidateSHA(commit); err != nil {
		return "", err
	}
	if strings.TrimSpace(message) == "" || strings.ContainsRune(message, 0) {
		return "", errors.New("tag message is empty or contains NUL")
	}
	_, path, err := c.path(input)
	if err != nil {
		return "", err
	}
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return "", err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return "", err
	}
	if err := c.assertCommit(ctx, path, commit); err != nil {
		return "", err
	}
	ref := "refs/tags/" + tag
	object, err := c.mirrorTag(ctx, path, ref, commit)
	if err != nil {
		return "", err
	}
	if object == "" {
		content := fmt.Sprintf("object %s\ntype commit\ntag %s\ntagger Oberth <oberth@localhost> %d +0000\n\n%s\n", commit, tag, time.Now().Unix(), strings.TrimRight(message, "\n"))
		created, err := c.execute(ctx, commandSpec{dir: path, args: []string{"mktag"}, stdin: strings.NewReader(content)}, true)
		if err != nil {
			return "", fmt.Errorf("create annotated tag %s: %w", tag, err)
		}
		object = strings.TrimSpace(created)
		if err := ValidateSHA(object); err != nil {
			return "", fmt.Errorf("create annotated tag %s: %w", tag, err)
		}
		// Create-only: a concurrent writer of the same ref fails this update.
		if err := c.run(ctx, commandSpec{dir: path, args: []string{"update-ref", ref, object, strings.Repeat("0", len(object))}}); err != nil {
			return "", fmt.Errorf("record annotated tag %s: %w", tag, err)
		}
	}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"push", "upstream", object + ":" + ref}}); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return "", fmt.Errorf("%w: upstream %s: %w", ErrTagExists, ref, err)
		}
		return "", fmt.Errorf("push tag %s without force: %w", tag, err)
	}
	if err := c.verifyRemoteRef(ctx, path, "--tags", ref, object); err != nil {
		return "", err
	}
	return object, nil
}

// mirrorTag returns the mirror's annotated tag object at ref when it peels
// to commit, "" when ref is absent, and ErrTagExists otherwise.
func (c *Cache) mirrorTag(ctx context.Context, path, ref, commit string) (string, error) {
	object, err := c.capture(ctx, path, "rev-parse", "--verify", "--quiet", ref)
	if err != nil || strings.TrimSpace(object) == "" {
		return "", nil //nolint:nilerr // --verify --quiet exits 1 for an absent ref.
	}
	object = strings.TrimSpace(object)
	kind, err := c.capture(ctx, path, "cat-file", "-t", object)
	if err != nil {
		return "", err
	}
	peeled, err := c.capture(ctx, path, "rev-parse", "--verify", object+"^{commit}")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(kind) != "tag" || strings.TrimSpace(peeled) != commit {
		return "", fmt.Errorf("%w: %s is %s %s", ErrTagExists, ref, strings.TrimSpace(kind), shortObject(strings.TrimSpace(peeled)))
	}
	return object, nil
}

func shortObject(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
