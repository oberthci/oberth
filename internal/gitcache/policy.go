package gitcache

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/gitport"
)

// policyHead reads the forge's current default head of repo and makes sure
// its object is in the cache.
func (c *Cache) policyHead(ctx context.Context, repo, p string, r Remote) (gitport.Ref, error) {
	head, err := c.remoteRef(ctx, p, r, r.DefaultBranch)
	if isAbsent(err) {
		return head, c.policyAbsence(ctx, p, r)
	}
	if err != nil {
		return head, err
	}
	return head, c.headObject(ctx, repo, r, head.ObjectSHA)
}

// headObject brings the default head's object into the cache. An object the
// cache has needs no transfer. A missing one arrives with the repository's
// refresh fetch, which finishes in the background when the forge has much to
// send, and the caller is told so when it stops waiting; a head the refresh
// did not bring (it moved since, or the refresh failed) is fetched on its own.
func (c *Cache) headObject(ctx context.Context, repo string, r Remote, oid string) error {
	p, err := c.path(repo)
	if err != nil {
		return err
	}
	if c.has(ctx, p, oid) {
		return nil
	}
	_, err = c.refreshQuietly(ctx, repo, false)
	var updating *gitport.FetchUnfinishedError
	if errors.As(err, &updating) {
		return err
	}
	if c.has(ctx, p, oid) {
		return nil
	}
	return c.fetchObject(ctx, p, r, oid)
}

// has reports whether the cache holds object oid; a failed check counts as
// absent, so the forge is asked.
func (c *Cache) has(ctx context.Context, p, oid string) bool {
	return c.run(ctx, command{dir: p, args: []string{"cat-file", "-e", oid}}) == nil
}

func (c *Cache) policyAbsence(ctx context.Context, p string, r Remote) error {
	data, err := c.capture(
		ctx,
		command{
			dir:    p,
			args:   []string{"ls-remote", "--", r.URL, "refs/heads/*", "refs/tags/*"},
			remote: &r,
		},
		2<<20,
	)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return &gitport.EmptyRepositoryError{}
	}
	return &RefusalError{reason: reasonDeletedDefault, remedy: remedyDeletedDefault, Field: r.DefaultBranch}
}
func (c *Cache) checkDefault(ctx context.Context, p, repo string, r Remote) error {
	_, err := c.resolve(ctx, p, repo, r.DefaultBranch)
	if isAbsent(err) {
		return c.policyAbsence(ctx, p, r)
	}
	return err
}
