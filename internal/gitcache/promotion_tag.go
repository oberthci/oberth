package gitcache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/pushretry"
)

// promotionTagger is the tagger of every tag_after tag object.
const promotionTagger = "Oberth <oberth@localhost>"

// ErrTagExists reports that refs/tags/<name> already names another commit,
// on the mirror or upstream. It is never transient.
var ErrTagExists = errors.New("tag already exists at another commit")

// PublishAnnotatedTag pushes an annotated tag on commit to the upstream
// without force, for promote tag_after (#1524), and returns the tag object.
// The tag object is created once on the mirror (tagger Oberth, the same
// identity as promotion merges) and reused by every retry; a mirror tag
// that this function created on commit is reused. Any other tag of that name,
// on the mirror or upstream, is ErrTagExists, and a tag the upstream refused
// is removed from the mirror. The remote ref is verified after the push.
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
		content := fmt.Sprintf("object %s\ntype commit\ntag %s\ntagger %s %d +0000\n\n%s\n", commit, tag, promotionTagger, time.Now().Unix(), strings.TrimRight(message, "\n"))
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
		exists := strings.Contains(strings.ToLower(err.Error()), "already exists")
		if exists || !pushretry.Transient(err) {
			// A refused tag never stays on the mirror: it would be served to
			// clients as a tag the upstream does not have, and would hold the
			// name against a later push. A transient failure keeps the object
			// for the retry.
			if dropErr := c.deleteRef(ctx, path, ref, object); dropErr != nil {
				err = errors.Join(err, fmt.Errorf("drop refused mirror tag %s: %w", tag, dropErr))
			}
		}
		if exists {
			return "", fmt.Errorf("%w: upstream %s: %w", ErrTagExists, ref, err)
		}
		return "", fmt.Errorf("push tag %s without force: %w", tag, err)
	}
	if err := c.verifyRemoteRef(ctx, path, "--tags", ref, object); err != nil {
		return "", err
	}
	return object, nil
}

// mirrorTag returns the mirror's annotated tag object at ref when tag_after
// created it on commit, "" when ref is absent, and ErrTagExists otherwise.
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
	// Only a tag object this function created is reused. Another writer's tag
	// on the same commit (a release tag still waiting for its release run) is
	// never pushed from here.
	body, err := c.capture(ctx, path, "cat-file", "tag", object)
	if err != nil {
		return "", err
	}
	header, _, _ := strings.Cut(body, "\n\n")
	if !strings.Contains(header+"\n", "\ntagger "+promotionTagger+" ") {
		return "", fmt.Errorf("%w: %s was not created by tag_after", ErrTagExists, ref)
	}
	return object, nil
}

func shortObject(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// UpstreamContainsCommit fetches the current target privately and checks the
// promoted result's ancestry. It never moves a public mirror ref.
func (c *Cache) UpstreamContainsCommit(ctx context.Context, input, branch, commit string) (bool, error) {
	if err := ValidateBranch(branch); err != nil {
		return false, err
	}
	if err := ValidateSHA(commit); err != nil {
		return false, err
	}
	_, path, err := c.path(input)
	if err != nil {
		return false, err
	}
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return false, err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return false, err
	}
	if err := c.configureRemote(ctx, input, path); err != nil {
		return false, err
	}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"fetch", "--no-tags", "upstream", "refs/heads/" + branch}}); err != nil {
		return false, err
	}
	err = c.run(ctx, commandSpec{dir: path, args: []string{"merge-base", "--is-ancestor", commit, "FETCH_HEAD"}})
	if err == nil {
		return true, nil
	}
	if isExitCode(err, 1) {
		return false, nil
	}
	return false, err
}
