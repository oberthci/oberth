package gitcache

import (
	"context"
	"errors"
	"os/exec"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

// Pin reads producer identity and fresh ancestry; it creates no retention refs.
// ReceiveSource uses the same read in quarantine without importing pushed data.
func (c *Cache) Pin(ctx context.Context, at sourcecontract.Revision) (int64, bool, error) {
	id, _, reachable, err := c.defaultAncestor(ctx, at)
	return id, reachable, err
}

// DefaultAncestor returns the exact default head used for the ancestry decision.
func (c *Cache) DefaultAncestor(ctx context.Context, at sourcecontract.Revision) (sourcecontract.Ref, bool, error) {
	_, head, reachable, err := c.defaultAncestor(ctx, at)
	return head, reachable, err
}

func (c *Cache) defaultAncestor(ctx context.Context, at sourcecontract.Revision) (int64, sourcecontract.Ref, bool, error) {
	if err := at.Validate(); err != nil {
		return 0, sourcecontract.Ref{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return 0, sourcecontract.Ref{}, false, context.Cause(ctx)
	}
	r, err := c.producer(ctx, at.Repository)
	if err != nil || r.RepositoryID == 0 {
		return 0, sourcecontract.Ref{}, false, err
	}
	p, err := c.path(at.Repository)
	if err != nil {
		return 0, sourcecontract.Ref{}, false, err
	}

	head, err := c.policyHead(ctx, p, r)
	if isAbsent(err) {
		return r.RepositoryID, sourcecontract.Ref{}, false, nil
	}
	if err != nil {
		return r.RepositoryID, sourcecontract.Ref{}, false, err
	}
	if err = c.commit(ctx, p, at.Commit); isAbsent(err) {
		return r.RepositoryID, sourcecontract.Ref{}, false, nil
	}
	if err != nil {
		return r.RepositoryID, sourcecontract.Ref{}, false, err
	}
	reachable, err := c.ancestor(ctx, p, at.Commit, head.CommitSHA)
	return r.RepositoryID, head, reachable, err
}

func (c *Cache) producer(ctx context.Context, repo string) (Remote, error) {
	if c.config.Upstream == nil {
		return Remote{}, nil
	}
	r, err := c.config.Upstream(ctx, repo)
	if err != nil {
		return Remote{}, unavailable("read producer registration", err)
	}
	if r.RepositoryID == 0 {
		return Remote{}, nil
	}
	if r.RepositoryID < 0 {
		return Remote{}, refuse("producer registration")
	}
	r, err = validateRemote(r)
	if err != nil {
		return r, err
	}
	return r, nil
}

func (c *Cache) ancestor(ctx context.Context, p, commit, head string) (bool, error) {
	err := c.run(ctx, command{dir: p, args: []string{"merge-base", "--is-ancestor", commit, head}})
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}
