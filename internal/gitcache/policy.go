package gitcache

import (
	"context"

	"github.com/oberthci/oberth/internal/gitport"
)

func (c *Cache) policyHead(ctx context.Context, p string, r Remote) (gitport.Ref, error) {
	head, err := c.remoteRef(ctx, p, r, r.DefaultBranch)
	if isAbsent(err) {
		return head, c.policyAbsence(ctx, p, r)
	}
	if err != nil {
		return head, err
	}
	err = c.fetchObject(ctx, p, r, head.ObjectSHA)
	return head, err
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
